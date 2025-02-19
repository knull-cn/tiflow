// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package entry

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	timeta "github.com/pingcap/tidb/meta"
	timodel "github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tiflow/cdc/model"
	cerror "github.com/pingcap/tiflow/pkg/errors"
	"github.com/pingcap/tiflow/pkg/filter"
	"github.com/pingcap/tiflow/pkg/retry"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// schemaSnapshot stores the source TiDB all schema information
// schemaSnapshot is a READ ONLY struct
type schemaSnapshot struct {
	tableNameToID  map[model.TableName]int64
	schemaNameToID map[string]int64

	schemas        map[int64]*timodel.DBInfo
	tables         map[int64]*model.TableInfo
	partitionTable map[int64]*model.TableInfo

	// key is schemaID and value is tableIDs
	tableInSchema map[int64][]int64

	truncateTableID   map[int64]struct{}
	ineligibleTableID map[int64]struct{}

	currentTs uint64

	// if forceReplicate is true, treat ineligible tables as eligible.
	forceReplicate bool
}

// SingleSchemaSnapshot is a single schema snapshot independent of schema storage
type SingleSchemaSnapshot = schemaSnapshot

// HandleDDL handles the ddl job
func (s *SingleSchemaSnapshot) HandleDDL(job *timodel.Job) error {
	return s.handleDDL(job)
}

// PreTableInfo returns the table info which will be overwritten by the specified job
func (s *SingleSchemaSnapshot) PreTableInfo(job *timodel.Job) (*model.TableInfo, error) {
	switch job.Type {
	case timodel.ActionCreateSchema, timodel.ActionModifySchemaCharsetAndCollate, timodel.ActionDropSchema:
		return nil, nil
	case timodel.ActionCreateTable, timodel.ActionCreateView, timodel.ActionRecoverTable:
		// no pre table info
		return nil, nil
	case timodel.ActionRenameTable, timodel.ActionDropTable, timodel.ActionDropView, timodel.ActionTruncateTable:
		// get the table will be dropped
		table, ok := s.TableByID(job.TableID)
		if !ok {
			return nil, cerror.ErrSchemaStorageTableMiss.GenWithStackByArgs(job.TableID)
		}
		return table, nil
	case timodel.ActionRenameTables:
		// DDL on multiple tables, ignore pre table info
		return nil, nil
	default:
		binlogInfo := job.BinlogInfo
		if binlogInfo == nil {
			log.Warn("ignore a invalid DDL job", zap.Reflect("job", job))
			return nil, nil
		}
		tbInfo := binlogInfo.TableInfo
		if tbInfo == nil {
			log.Warn("ignore a invalid DDL job", zap.Reflect("job", job))
			return nil, nil
		}
		tableID := tbInfo.ID
		table, ok := s.TableByID(tableID)
		if !ok {
			return nil, cerror.ErrSchemaStorageTableMiss.GenWithStackByArgs(job.TableID)
		}
		return table, nil
	}
}

// NewSingleSchemaSnapshotFromMeta creates a new single schema snapshot from a tidb meta
func NewSingleSchemaSnapshotFromMeta(meta *timeta.Meta, currentTs uint64, forceReplicate bool) (*SingleSchemaSnapshot, error) {
	// meta is nil only in unit tests
	if meta == nil {
		snap := newEmptySchemaSnapshot(forceReplicate)
		snap.currentTs = currentTs
		return snap, nil
	}
	return newSchemaSnapshotFromMeta(meta, currentTs, forceReplicate)
}

func newEmptySchemaSnapshot(forceReplicate bool) *schemaSnapshot {
	return &schemaSnapshot{
		tableNameToID:  make(map[model.TableName]int64),
		schemaNameToID: make(map[string]int64),

		schemas:        make(map[int64]*timodel.DBInfo),
		tables:         make(map[int64]*model.TableInfo),
		partitionTable: make(map[int64]*model.TableInfo),

		tableInSchema:     make(map[int64][]int64),
		truncateTableID:   make(map[int64]struct{}),
		ineligibleTableID: make(map[int64]struct{}),

		forceReplicate: forceReplicate,
	}
}

func newSchemaSnapshotFromMeta(meta *timeta.Meta, currentTs uint64, forceReplicate bool) (*schemaSnapshot, error) {
	snap := newEmptySchemaSnapshot(forceReplicate)
	dbinfos, err := meta.ListDatabases()
	if err != nil {
		return nil, cerror.WrapError(cerror.ErrMetaListDatabases, err)
	}
	for _, dbinfo := range dbinfos {
		snap.schemas[dbinfo.ID] = dbinfo
		snap.schemaNameToID[dbinfo.Name.O] = dbinfo.ID
	}
	for schemaID, dbinfo := range snap.schemas {
		tableInfos, err := meta.ListTables(schemaID)
		if err != nil {
			return nil, cerror.WrapError(cerror.ErrMetaListDatabases, err)
		}
		snap.tableInSchema[schemaID] = make([]int64, 0, len(tableInfos))
		for _, tableInfo := range tableInfos {
			snap.tableInSchema[schemaID] = append(snap.tableInSchema[schemaID], tableInfo.ID)
			tableInfo := model.WrapTableInfo(dbinfo.ID, dbinfo.Name.O, currentTs, tableInfo)
			snap.tables[tableInfo.ID] = tableInfo
			snap.tableNameToID[model.TableName{Schema: dbinfo.Name.O, Table: tableInfo.Name.O}] = tableInfo.ID
			isEligible := tableInfo.IsEligible(forceReplicate)
			if !isEligible {
				snap.ineligibleTableID[tableInfo.ID] = struct{}{}
			}
			if pi := tableInfo.GetPartitionInfo(); pi != nil {
				for _, partition := range pi.Definitions {
					snap.partitionTable[partition.ID] = tableInfo
					if !isEligible {
						snap.ineligibleTableID[partition.ID] = struct{}{}
					}
				}
			}
		}
	}
	snap.currentTs = currentTs
	return snap, nil
}

func (s *schemaSnapshot) PrintStatus(logger func(msg string, fields ...zap.Field)) {
	logger("[SchemaSnap] Start to print status", zap.Uint64("currentTs", s.currentTs))
	for id, dbInfo := range s.schemas {
		logger("[SchemaSnap] --> Schemas", zap.Int64("schemaID", id), zap.Reflect("dbInfo", dbInfo))
		// check schemaNameToID
		if schemaID, exist := s.schemaNameToID[dbInfo.Name.O]; !exist || schemaID != id {
			logger("[SchemaSnap] ----> schemaNameToID item lost", zap.String("name", dbInfo.Name.O), zap.Int64("schemaNameToID", s.schemaNameToID[dbInfo.Name.O]))
		}
	}
	if len(s.schemaNameToID) != len(s.schemas) {
		logger("[SchemaSnap] schemaNameToID length mismatch schemas")
		for schemaName, schemaID := range s.schemaNameToID {
			logger("[SchemaSnap] --> schemaNameToID", zap.String("schemaName", schemaName), zap.Int64("schemaID", schemaID))
		}
	}
	for id, tableInfo := range s.tables {
		logger("[SchemaSnap] --> Tables", zap.Int64("tableID", id), zap.Stringer("tableInfo", tableInfo))
		// check tableNameToID
		if tableID, exist := s.tableNameToID[tableInfo.TableName]; !exist || tableID != id {
			logger("[SchemaSnap] ----> tableNameToID item lost", zap.Stringer("name", tableInfo.TableName), zap.Int64("tableNameToID", s.tableNameToID[tableInfo.TableName]))
		}
	}
	if len(s.tableNameToID) != len(s.tables) {
		logger("[SchemaSnap] tableNameToID length mismatch tables")
		for tableName, tableID := range s.tableNameToID {
			logger("[SchemaSnap] --> tableNameToID", zap.Stringer("tableName", tableName), zap.Int64("tableID", tableID))
		}
	}
	for pid, table := range s.partitionTable {
		logger("[SchemaSnap] --> Partitions", zap.Int64("partitionID", pid), zap.Int64("tableID", table.ID))
	}
	truncateTableID := make([]int64, 0, len(s.truncateTableID))
	for id := range s.truncateTableID {
		truncateTableID = append(truncateTableID, id)
	}
	logger("[SchemaSnap] TruncateTableIDs", zap.Int64s("ids", truncateTableID))

	ineligibleTableID := make([]int64, 0, len(s.ineligibleTableID))
	for id := range s.ineligibleTableID {
		ineligibleTableID = append(ineligibleTableID, id)
	}
	logger("[SchemaSnap] IneligibleTableIDs", zap.Int64s("ids", ineligibleTableID))
}

// Clone clones Storage
func (s *schemaSnapshot) Clone() *schemaSnapshot {
	clone := *s

	tableNameToID := make(map[model.TableName]int64, len(s.tableNameToID))
	for k, v := range s.tableNameToID {
		tableNameToID[k] = v
	}
	clone.tableNameToID = tableNameToID

	schemaNameToID := make(map[string]int64, len(s.schemaNameToID))
	for k, v := range s.schemaNameToID {
		schemaNameToID[k] = v
	}
	clone.schemaNameToID = schemaNameToID

	schemas := make(map[int64]*timodel.DBInfo, len(s.schemas))
	for k, v := range s.schemas {
		// DBInfo is readonly in TiCDC, shallow copy to reduce memory
		schemas[k] = v.Copy()
	}
	clone.schemas = schemas

	tables := make(map[int64]*model.TableInfo, len(s.tables))
	for k, v := range s.tables {
		tables[k] = v
	}
	clone.tables = tables

	tableInSchema := make(map[int64][]int64, len(s.tableInSchema))
	for k, v := range s.tableInSchema {
		cloneV := make([]int64, len(v))
		copy(cloneV, v)
		tableInSchema[k] = cloneV
	}
	clone.tableInSchema = tableInSchema

	partitionTable := make(map[int64]*model.TableInfo, len(s.partitionTable))
	for k, v := range s.partitionTable {
		partitionTable[k] = v
	}
	clone.partitionTable = partitionTable

	truncateTableID := make(map[int64]struct{}, len(s.truncateTableID))
	for k, v := range s.truncateTableID {
		truncateTableID[k] = v
	}
	clone.truncateTableID = truncateTableID

	ineligibleTableID := make(map[int64]struct{}, len(s.ineligibleTableID))
	for k, v := range s.ineligibleTableID {
		ineligibleTableID[k] = v
	}
	clone.ineligibleTableID = ineligibleTableID

	return &clone
}

// GetTableNameByID looks up a TableName with the given table id
func (s *schemaSnapshot) GetTableNameByID(id int64) (model.TableName, bool) {
	tableInfo, ok := s.tables[id]
	if !ok {
		// Try partition, it could be a partition table.
		partInfo, ok := s.partitionTable[id]
		if !ok {
			return model.TableName{}, false
		}
		// Must exists an table that contains the partition.
		tableInfo = s.tables[partInfo.ID]
	}
	return tableInfo.TableName, true
}

// GetTableIDByName returns the tableID by table schemaName and tableName
func (s *schemaSnapshot) GetTableIDByName(schemaName string, tableName string) (int64, bool) {
	id, ok := s.tableNameToID[model.TableName{
		Schema: schemaName,
		Table:  tableName,
	}]
	return id, ok
}

// GetTableByName queries a table by name,
// the second returned value is false if no table with the specified name is found.
func (s *schemaSnapshot) GetTableByName(schema, table string) (info *model.TableInfo, ok bool) {
	id, ok := s.GetTableIDByName(schema, table)
	if !ok {
		return nil, ok
	}
	return s.TableByID(id)
}

// SchemaByID returns the DBInfo by schema id
func (s *schemaSnapshot) SchemaByID(id int64) (val *timodel.DBInfo, ok bool) {
	val, ok = s.schemas[id]
	return
}

// SchemaByTableID returns the schema ID by table ID
func (s *schemaSnapshot) SchemaByTableID(tableID int64) (*timodel.DBInfo, bool) {
	tableInfo, ok := s.tables[tableID]
	if !ok {
		return nil, false
	}
	schemaID, ok := s.schemaNameToID[tableInfo.TableName.Schema]
	if !ok {
		return nil, false
	}
	return s.SchemaByID(schemaID)
}

// TableByID returns the TableInfo by table id
func (s *schemaSnapshot) TableByID(id int64) (val *model.TableInfo, ok bool) {
	val, ok = s.tables[id]
	return
}

// PhysicalTableByID returns the TableInfo by table id or partition ID.
func (s *schemaSnapshot) PhysicalTableByID(id int64) (val *model.TableInfo, ok bool) {
	val, ok = s.tables[id]
	if !ok {
		val, ok = s.partitionTable[id]
	}
	return
}

// IsTruncateTableID returns true if the table id have been truncated by truncate table DDL
func (s *schemaSnapshot) IsTruncateTableID(id int64) bool {
	_, ok := s.truncateTableID[id]
	return ok
}

// IsIneligibleTableID returns true if the table is ineligible
func (s *schemaSnapshot) IsIneligibleTableID(id int64) bool {
	_, ok := s.ineligibleTableID[id]
	return ok
}

// FillSchemaName fills the schema name in ddl job
func (s *schemaSnapshot) FillSchemaName(job *timodel.Job) error {
	if job.Type == timodel.ActionRenameTables {
		// DDLs on multiple schema or tables, ignore them.
		return nil
	}
	if job.Type == timodel.ActionCreateSchema ||
		job.Type == timodel.ActionDropSchema {
		job.SchemaName = job.BinlogInfo.DBInfo.Name.O
		return nil
	}
	dbInfo, exist := s.SchemaByID(job.SchemaID)
	if !exist {
		return cerror.ErrSnapshotSchemaNotFound.GenWithStackByArgs(job.SchemaID)
	}
	job.SchemaName = dbInfo.Name.O
	return nil
}

func (s *schemaSnapshot) dropSchema(id int64) error {
	schema, ok := s.schemas[id]
	if !ok {
		return cerror.ErrSnapshotSchemaNotFound.GenWithStackByArgs(id)
	}

	for _, tableID := range s.tableInSchema[id] {
		tableName := s.tables[tableID].TableName
		if pi := s.tables[tableID].GetPartitionInfo(); pi != nil {
			for _, partition := range pi.Definitions {
				delete(s.partitionTable, partition.ID)
			}
		}
		delete(s.tables, tableID)
		delete(s.tableNameToID, tableName)
	}

	delete(s.schemas, id)
	delete(s.tableInSchema, id)
	delete(s.schemaNameToID, schema.Name.O)

	return nil
}

func (s *schemaSnapshot) createSchema(db *timodel.DBInfo) error {
	if _, ok := s.schemas[db.ID]; ok {
		return cerror.ErrSnapshotSchemaExists.GenWithStackByArgs(db.Name, db.ID)
	}

	s.schemas[db.ID] = db.Copy()
	s.schemaNameToID[db.Name.O] = db.ID
	s.tableInSchema[db.ID] = []int64{}

	log.Debug("create schema success, schema id", zap.String("name", db.Name.O), zap.Int64("id", db.ID))
	return nil
}

func (s *schemaSnapshot) replaceSchema(db *timodel.DBInfo) error {
	_, ok := s.schemas[db.ID]
	if !ok {
		return cerror.ErrSnapshotSchemaNotFound.GenWithStack("schema %s(%d) not found", db.Name, db.ID)
	}
	s.schemas[db.ID] = db.Copy()
	s.schemaNameToID[db.Name.O] = db.ID
	return nil
}

func (s *schemaSnapshot) dropTable(id int64) error {
	table, ok := s.tables[id]
	if !ok {
		return cerror.ErrSnapshotTableNotFound.GenWithStackByArgs(id)
	}
	tableInSchema, ok := s.tableInSchema[table.SchemaID]
	if !ok {
		return cerror.ErrSnapshotSchemaNotFound.GenWithStack("table(%d)'s schema", id)
	}

	for i, tableID := range tableInSchema {
		if tableID == id {
			copy(tableInSchema[i:], tableInSchema[i+1:])
			s.tableInSchema[table.SchemaID] = tableInSchema[:len(tableInSchema)-1]
			break
		}
	}

	tableName := s.tables[id].TableName
	delete(s.tables, id)
	if pi := table.GetPartitionInfo(); pi != nil {
		for _, partition := range pi.Definitions {
			delete(s.partitionTable, partition.ID)
			delete(s.ineligibleTableID, partition.ID)
		}
	}
	delete(s.tableNameToID, tableName)
	delete(s.ineligibleTableID, id)

	log.Debug("drop table success", zap.String("name", table.Name.O), zap.Int64("id", id))
	return nil
}

func (s *schemaSnapshot) updatePartition(tbl *model.TableInfo) error {
	id := tbl.ID
	table, ok := s.tables[id]
	if !ok {
		return cerror.ErrSnapshotTableNotFound.GenWithStackByArgs(id)
	}
	oldPi := table.GetPartitionInfo()
	if oldPi == nil {
		return cerror.ErrSnapshotTableNotFound.GenWithStack("table %d is not a partition table", id)
	}
	oldIDs := make(map[int64]struct{}, len(oldPi.Definitions))
	for _, p := range oldPi.Definitions {
		oldIDs[p.ID] = struct{}{}
	}

	newPi := tbl.GetPartitionInfo()
	if newPi == nil {
		return cerror.ErrSnapshotTableNotFound.GenWithStack("table %d is not a partition table", id)
	}
	s.tables[id] = tbl
	for _, partition := range newPi.Definitions {
		// update table info.
		if _, ok := s.partitionTable[partition.ID]; ok {
			log.Debug("add table partition success",
				zap.String("name", tbl.Name.O), zap.Int64("tid", id),
				zap.Int64("add partition id", partition.ID))
		}
		s.partitionTable[partition.ID] = tbl
		if !tbl.IsEligible(s.forceReplicate) {
			s.ineligibleTableID[partition.ID] = struct{}{}
		}
		delete(oldIDs, partition.ID)
	}

	// drop old partition.
	for pid := range oldIDs {
		s.truncateTableID[pid] = struct{}{}
		delete(s.partitionTable, pid)
		delete(s.ineligibleTableID, pid)
		log.Debug("drop table partition success",
			zap.String("name", tbl.Name.O), zap.Int64("tid", id),
			zap.Int64("truncated partition id", pid))
	}

	return nil
}

func (s *schemaSnapshot) createTable(table *model.TableInfo) error {
	schema, ok := s.schemas[table.SchemaID]
	if !ok {
		return cerror.ErrSnapshotSchemaNotFound.GenWithStack("table's schema(%d)", table.SchemaID)
	}
	tableInSchema, ok := s.tableInSchema[table.SchemaID]
	if !ok {
		return cerror.ErrSnapshotSchemaNotFound.GenWithStack("table's schema(%d)", table.SchemaID)
	}
	_, ok = s.tables[table.ID]
	if ok {
		return cerror.ErrSnapshotTableExists.GenWithStackByArgs(schema.Name, table.Name)
	}
	tableInSchema = append(tableInSchema, table.ID)
	s.tableInSchema[table.SchemaID] = tableInSchema

	s.tables[table.ID] = table
	if !table.IsEligible(s.forceReplicate) {
		// Sequence is not supported yet, and always ineligible.
		// Skip Warn to avoid confusion.
		// See https://github.com/pingcap/tiflow/issues/4559
		if !table.IsSequence() {
			log.Warn("this table is ineligible to replicate",
				zap.String("tableName", table.Name.O), zap.Int64("tableID", table.ID))
		}
		s.ineligibleTableID[table.ID] = struct{}{}
	}
	if pi := table.GetPartitionInfo(); pi != nil {
		for _, partition := range pi.Definitions {
			s.partitionTable[partition.ID] = table
			if !table.IsEligible(s.forceReplicate) {
				s.ineligibleTableID[partition.ID] = struct{}{}
			}
		}
	}
	s.tableNameToID[table.TableName] = table.ID

	log.Debug("create table success", zap.String("name", schema.Name.O+"."+table.Name.O), zap.Int64("id", table.ID))
	return nil
}

// ReplaceTable replace the table by new tableInfo
func (s *schemaSnapshot) replaceTable(table *model.TableInfo) error {
	_, ok := s.tables[table.ID]
	if !ok {
		return cerror.ErrSnapshotTableNotFound.GenWithStack("table %s(%d)", table.Name, table.ID)
	}
	s.tables[table.ID] = table
	if !table.IsEligible(s.forceReplicate) {
		// Sequence is not supported yet, and always ineligible.
		// Skip Warn to avoid confusion.
		// See https://github.com/pingcap/tiflow/issues/4559
		if !table.IsSequence() {
			log.Warn("this table is ineligible to replicate",
				zap.String("tableName", table.Name.O), zap.Int64("tableID", table.ID))
		}
		s.ineligibleTableID[table.ID] = struct{}{}
	}
	if pi := table.GetPartitionInfo(); pi != nil {
		for _, partition := range pi.Definitions {
			s.partitionTable[partition.ID] = table
			if !table.IsEligible(s.forceReplicate) {
				s.ineligibleTableID[partition.ID] = struct{}{}
			}
		}
	}

	return nil
}

func (s *schemaSnapshot) handleDDL(job *timodel.Job) error {
	if err := s.FillSchemaName(job); err != nil {
		return errors.Trace(err)
	}
	getWrapTableInfo := func(job *timodel.Job) *model.TableInfo {
		return model.WrapTableInfo(job.SchemaID, job.SchemaName,
			job.BinlogInfo.FinishedTS,
			job.BinlogInfo.TableInfo)
	}
	switch job.Type {
	case timodel.ActionCreateSchema:
		// get the DBInfo from job rawArgs
		err := s.createSchema(job.BinlogInfo.DBInfo)
		if err != nil {
			return errors.Trace(err)
		}
	case timodel.ActionModifySchemaCharsetAndCollate:
		err := s.replaceSchema(job.BinlogInfo.DBInfo)
		if err != nil {
			return errors.Trace(err)
		}
	case timodel.ActionDropSchema:
		err := s.dropSchema(job.SchemaID)
		if err != nil {
			return errors.Trace(err)
		}
	case timodel.ActionRenameTable:
		// first drop the table
		err := s.dropTable(job.TableID)
		if err != nil {
			return errors.Trace(err)
		}
		// create table
		err = s.createTable(getWrapTableInfo(job))
		if err != nil {
			return errors.Trace(err)
		}
	case timodel.ActionRenameTables:
		err := s.renameTables(job)
		if err != nil {
			return errors.Trace(err)
		}
	case timodel.ActionCreateTable, timodel.ActionCreateView, timodel.ActionRecoverTable:
		err := s.createTable(getWrapTableInfo(job))
		if err != nil {
			return errors.Trace(err)
		}
	case timodel.ActionDropTable, timodel.ActionDropView:
		err := s.dropTable(job.TableID)
		if err != nil {
			return errors.Trace(err)
		}

	case timodel.ActionTruncateTable:
		// job.TableID is the old table id, different from table.ID
		err := s.dropTable(job.TableID)
		if err != nil {
			return errors.Trace(err)
		}

		err = s.createTable(getWrapTableInfo(job))
		if err != nil {
			return errors.Trace(err)
		}

		s.truncateTableID[job.TableID] = struct{}{}
	case timodel.ActionTruncateTablePartition, timodel.ActionAddTablePartition, timodel.ActionDropTablePartition:
		err := s.updatePartition(getWrapTableInfo(job))
		if err != nil {
			return errors.Trace(err)
		}
	default:
		binlogInfo := job.BinlogInfo
		if binlogInfo == nil {
			log.Warn("ignore a invalid DDL job", zap.Reflect("job", job))
			return nil
		}
		tbInfo := binlogInfo.TableInfo
		if tbInfo == nil {
			log.Warn("ignore a invalid DDL job", zap.Reflect("job", job))
			return nil
		}
		err := s.replaceTable(getWrapTableInfo(job))
		if err != nil {
			return errors.Trace(err)
		}
	}
	s.currentTs = job.BinlogInfo.FinishedTS
	return nil
}

func (s *schemaSnapshot) renameTables(job *timodel.Job) error {
	var oldSchemaIDs, newSchemaIDs, oldTableIDs []int64
	var newTableNames, oldSchemaNames []*timodel.CIStr
	err := job.DecodeArgs(&oldSchemaIDs, &newSchemaIDs, &newTableNames, &oldTableIDs, &oldSchemaNames)
	if err != nil {
		return errors.Trace(err)
	}
	if len(job.BinlogInfo.MultipleTableInfos) < len(newTableNames) {
		return cerror.ErrInvalidDDLJob.GenWithStackByArgs(job.ID)
	}
	// NOTE: should handle failures in halfway better.
	for _, tableID := range oldTableIDs {
		if err := s.dropTable(tableID); err != nil {
			return errors.Trace(err)
		}
	}
	for i, tableInfo := range job.BinlogInfo.MultipleTableInfos {
		newSchema, ok := s.SchemaByID(newSchemaIDs[i])
		if !ok {
			return cerror.ErrSnapshotSchemaNotFound.GenWithStackByArgs(newSchemaIDs[i])
		}
		newSchemaName := newSchema.Name.L
		err = s.createTable(model.WrapTableInfo(
			newSchemaIDs[i], newSchemaName, job.BinlogInfo.FinishedTS, tableInfo))
		if err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

// CloneTables return a clone of the existing tables.
func (s *schemaSnapshot) CloneTables() map[model.TableID]model.TableName {
	mp := make(map[model.TableID]model.TableName, len(s.tables))

	for id, table := range s.tables {
		mp[id] = table.TableName
	}

	return mp
}

// Tables return a map between table id and table info
// the returned map must be READ-ONLY. Any modified of this map will lead to the internal state confusion in schema storage
func (s *schemaSnapshot) Tables() map[model.TableID]*model.TableInfo {
	return s.tables
}

// SchemaStorage stores the schema information with multi-version
type SchemaStorage interface {
	// GetSnapshot returns the snapshot which of ts is specified.
	// It may block caller when ts is larger than ResolvedTs.
	GetSnapshot(ctx context.Context, ts uint64) (*SingleSchemaSnapshot, error)
	// GetLastSnapshot returns the last snapshot
	GetLastSnapshot() *schemaSnapshot
	// HandleDDLJob creates a new snapshot in storage and handles the ddl job
	HandleDDLJob(job *timodel.Job) error
	// AdvanceResolvedTs advances the resolved
	AdvanceResolvedTs(ts uint64)
	// ResolvedTs returns the resolved ts of the schema storage
	ResolvedTs() uint64
	// DoGC removes snaps that are no longer needed at the specified TS.
	// It returns the TS from which the oldest maintained snapshot is valid.
	DoGC(ts uint64) (lastSchemaTs uint64)
}

type schemaStorageImpl struct {
	snaps      []*schemaSnapshot
	snapsMu    sync.RWMutex
	gcTs       uint64
	resolvedTs uint64

	filter         *filter.Filter
	forceReplicate bool

	id model.ChangeFeedID
}

// NewSchemaStorage creates a new schema storage
func NewSchemaStorage(
	meta *timeta.Meta, startTs uint64, filter *filter.Filter,
	forceReplicate bool, id model.ChangeFeedID,
) (SchemaStorage, error) {
	var snap *schemaSnapshot
	var err error
	if meta == nil {
		snap = newEmptySchemaSnapshot(forceReplicate)
	} else {
		snap, err = newSchemaSnapshotFromMeta(meta, startTs, forceReplicate)
	}
	if err != nil {
		return nil, errors.Trace(err)
	}
	schema := &schemaStorageImpl{
		snaps:          []*schemaSnapshot{snap},
		resolvedTs:     startTs,
		filter:         filter,
		forceReplicate: forceReplicate,
		id:             id,
	}
	return schema, nil
}

func (s *schemaStorageImpl) getSnapshot(ts uint64) (*schemaSnapshot, error) {
	gcTs := atomic.LoadUint64(&s.gcTs)
	if ts < gcTs {
		// Unexpected error, caller should fail immediately.
		return nil, cerror.ErrSchemaStorageGCed.GenWithStackByArgs(ts, gcTs)
	}
	resolvedTs := atomic.LoadUint64(&s.resolvedTs)
	if ts > resolvedTs {
		// Caller should retry.
		return nil, cerror.ErrSchemaStorageUnresolved.GenWithStackByArgs(ts, resolvedTs)
	}
	s.snapsMu.RLock()
	defer s.snapsMu.RUnlock()
	i := sort.Search(len(s.snaps), func(i int) bool {
		return s.snaps[i].currentTs > ts
	})
	if i <= 0 {
		// Unexpected error, caller should fail immediately.
		return nil, cerror.ErrSchemaSnapshotNotFound.GenWithStackByArgs(ts)
	}
	return s.snaps[i-1], nil
}

// GetSnapshot returns the snapshot which of ts is specified
func (s *schemaStorageImpl) GetSnapshot(ctx context.Context, ts uint64) (*schemaSnapshot, error) {
	var snap *schemaSnapshot

	// The infinite retry here is a temporary solution to the `ErrSchemaStorageUnresolved` caused by
	// DDL puller lagging too much.
	startTime := time.Now()
	logTime := startTime
	err := retry.Do(ctx, func() error {
		var err error
		snap, err = s.getSnapshot(ts)
		now := time.Now()
		if now.Sub(logTime) >= 30*time.Second && isRetryable(err) {
			log.Warn("GetSnapshot is taking too long, DDL puller stuck?",
				zap.Uint64("ts", ts),
				zap.Duration("duration", now.Sub(startTime)),
				zap.String("changefeed", s.id))
			logTime = now
		}
		return err
	}, retry.WithBackoffBaseDelay(10), retry.WithInfiniteTries(),
		retry.WithIsRetryableErr(isRetryable))

	return snap, err
}

func isRetryable(err error) bool {
	return cerror.IsRetryableError(err) && cerror.ErrSchemaStorageUnresolved.Equal(err)
}

// GetLastSnapshot returns the last snapshot
func (s *schemaStorageImpl) GetLastSnapshot() *schemaSnapshot {
	s.snapsMu.RLock()
	defer s.snapsMu.RUnlock()
	return s.snaps[len(s.snaps)-1]
}

// HandleDDLJob creates a new snapshot in storage and handles the ddl job
func (s *schemaStorageImpl) HandleDDLJob(job *timodel.Job) error {
	if s.skipJob(job) {
		s.AdvanceResolvedTs(job.BinlogInfo.FinishedTS)
		return nil
	}
	s.snapsMu.Lock()
	defer s.snapsMu.Unlock()
	var snap *schemaSnapshot
	if len(s.snaps) > 0 {
		lastSnap := s.snaps[len(s.snaps)-1]
		if job.BinlogInfo.FinishedTS <= lastSnap.currentTs {
			log.Info("ignore foregone DDL", zap.Int64("jobID", job.ID),
				zap.String("DDL", job.Query), zap.String("changefeed", s.id),
				zap.Uint64("finishTs", job.BinlogInfo.FinishedTS))
			return nil
		}
		snap = lastSnap.Clone()
	} else {
		snap = newEmptySchemaSnapshot(s.forceReplicate)
	}
	if err := snap.handleDDL(job); err != nil {
		log.Error("handle DDL failed", zap.String("DDL", job.Query),
			zap.Stringer("job", job), zap.Error(err),
			zap.String("changefeed", s.id), zap.Uint64("finishTs", job.BinlogInfo.FinishedTS))
		return errors.Trace(err)
	}
	log.Info("handle DDL", zap.String("DDL", job.Query),
		zap.Stringer("job", job), zap.String("changefeed", s.id),
		zap.Uint64("finishTs", job.BinlogInfo.FinishedTS))

	s.snaps = append(s.snaps, snap)
	s.AdvanceResolvedTs(job.BinlogInfo.FinishedTS)
	return nil
}

// AdvanceResolvedTs advances the resolved
func (s *schemaStorageImpl) AdvanceResolvedTs(ts uint64) {
	var swapped bool
	for !swapped {
		oldResolvedTs := atomic.LoadUint64(&s.resolvedTs)
		if ts < oldResolvedTs {
			return
		}
		swapped = atomic.CompareAndSwapUint64(&s.resolvedTs, oldResolvedTs, ts)
	}
}

// ResolvedTs returns the resolved ts of the schema storage
func (s *schemaStorageImpl) ResolvedTs() uint64 {
	return atomic.LoadUint64(&s.resolvedTs)
}

// DoGC removes snaps which of ts less than this specified ts
func (s *schemaStorageImpl) DoGC(ts uint64) (lastSchemaTs uint64) {
	s.snapsMu.Lock()
	defer s.snapsMu.Unlock()
	var startIdx int
	for i, snap := range s.snaps {
		if snap.currentTs > ts {
			break
		}
		startIdx = i
	}
	if startIdx == 0 {
		return s.snaps[0].currentTs
	}
	if log.GetLevel() == zapcore.DebugLevel {
		log.Debug("Do GC in schema storage")
		for i := 0; i < startIdx; i++ {
			s.snaps[i].PrintStatus(log.Debug)
		}
	}

	// copy the part of the slice that is needed instead of re-slicing it
	// to maximize efficiency of Go runtime GC.
	newSnaps := make([]*schemaSnapshot, len(s.snaps)-startIdx)
	copy(newSnaps, s.snaps[startIdx:])
	s.snaps = newSnaps

	lastSchemaTs = s.snaps[0].currentTs
	atomic.StoreUint64(&s.gcTs, lastSchemaTs)
	return
}

// SkipJob skip the job should not be executed
// TiDB write DDL Binlog for every DDL Job, we must ignore jobs that are cancelled or rollback
// For older version TiDB, it write DDL Binlog in the txn that the state of job is changed to *synced*
// Now, it write DDL Binlog in the txn that the state of job is changed to *done* (before change to *synced*)
// At state *done*, it will be always and only changed to *synced*.
func (s *schemaStorageImpl) skipJob(job *timodel.Job) bool {
	log.Debug("handle DDL new commit",
		zap.String("DDL", job.Query), zap.Stringer("job", job),
		zap.String("changefeed", s.id))
	if s.filter != nil && s.filter.ShouldDiscardDDL(job.Type) {
		log.Info("discard DDL",
			zap.Int64("jobID", job.ID), zap.String("DDL", job.Query),
			zap.String("changefeed", s.id))
		return true
	}
	return !job.IsSynced() && !job.IsDone()
}
