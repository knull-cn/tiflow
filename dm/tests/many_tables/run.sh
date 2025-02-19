#!/bin/bash

set -eu

cur=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source $cur/../_utils/test_prepare

WORK_DIR=$TEST_DIR/$TEST_NAME
TABLE_NUM=500

function prepare_data() {
	run_sql 'DROP DATABASE if exists many_tables_db;' $MYSQL_PORT1 $MYSQL_PASSWORD1
	run_sql 'CREATE DATABASE many_tables_db;' $MYSQL_PORT1 $MYSQL_PASSWORD1
	for i in $(seq $TABLE_NUM); do
		run_sql "CREATE TABLE many_tables_db.t$i(i TINYINT, j INT UNIQUE KEY);" $MYSQL_PORT1 $MYSQL_PASSWORD1
		for j in $(seq 2); do
			run_sql "INSERT INTO many_tables_db.t$i VALUES ($j,${j}000$j),($j,${j}001$j);" $MYSQL_PORT1 $MYSQL_PASSWORD1
		done
	done
}

function incremental_data() {
	for j in $(seq 3 5); do
		for i in $(seq $TABLE_NUM); do
			run_sql "INSERT INTO many_tables_db.t$i VALUES ($j,${j}000$j),($j,${j}001$j);" $MYSQL_PORT1 $MYSQL_PASSWORD1
		done
	done
}

function run() {
	echo "start prepare_data"
	prepare_data
	echo "finish prepare_data"

	run_dm_master $WORK_DIR/master $MASTER_PORT $cur/conf/dm-master.toml
	check_rpc_alive $cur/../bin/check_master_online 127.0.0.1:$MASTER_PORT
	run_dm_worker $WORK_DIR/worker1 $WORKER1_PORT $cur/conf/dm-worker1.toml
	check_rpc_alive $cur/../bin/check_worker_online 127.0.0.1:$WORKER1_PORT
	# operate mysql config to worker
	cp $cur/conf/source1.yaml $WORK_DIR/source1.yaml
	sed -i "/relay-binlog-name/i\relay-dir: $WORK_DIR/worker1/relay_log" $WORK_DIR/source1.yaml
	dmctl_operate_source create $WORK_DIR/source1.yaml $SOURCE_ID1

	dmctl_start_task_standalone
	run_dm_ctl_with_retry $WORK_DIR "127.0.0.1:$MASTER_PORT" \
		"query-status test" \
		"\"totalTables\": \"500\"" 1 \
		"\"completedTables\"" 1 \
		"\"finishedBytes\"" 1 \
		"\"finishedRows\"" 1 \
		"\"estimateTotalRows\"" 1
	wait_until_sync $WORK_DIR "127.0.0.1:$MASTER_PORT"
	check_sync_diff $WORK_DIR $cur/conf/diff_config.toml

	# check https://github.com/pingcap/tiflow/issues/5063
	check_time=20
	sleep 5
	while [ $check_time -gt 0 ]; do
		syncer_recv_event_num=$(grep '"receive binlog event"' $WORK_DIR/worker1/log/dm-worker.log | wc -l)
		if [ $syncer_recv_event_num -eq 3 ]; then
			break
		fi
		echo "syncer_recv_event_num: $syncer_recv_event_num, will retry later"
		sleep 1
		((check_time--))
	done

	if [ $syncer_recv_event_num -ne 3 ]; then
		exit 1
	fi

	echo "start incremental_data"
	incremental_data
	echo "finish incremental_data"

	check_sync_diff $WORK_DIR $cur/conf/diff_config.toml
}

cleanup_data many_tables_db
cleanup_process

run $*

cleanup_process

echo "[$(date)] <<<<<< test case $TEST_NAME success! >>>>>>"
