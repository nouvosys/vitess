/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package schemamanager

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	"vitess.io/vitess/go/vt/topo/memorytopo"

	"vitess.io/vitess/go/vt/logutil"
	"vitess.io/vitess/go/vt/mysqlctl/tmutils"
	tabletmanagerdatapb "vitess.io/vitess/go/vt/proto/tabletmanagerdata"
	"vitess.io/vitess/go/vt/schema"
	"vitess.io/vitess/go/vt/sqlparser"
)

var (
	testWaitReplicasTimeout = 10 * time.Second
)

func TestTabletExecutorOpen(t *testing.T) {
	executor := newFakeExecutor(t)
	ctx := context.Background()

	err := executor.Open(ctx, "test_keyspace")
	require.NoError(t, err)

	defer executor.Close()

	err = executor.Open(ctx, "test_keyspace")
	require.NoError(t, err, "open an opened executor should also succeed")
}

func TestTabletExecutorOpenWithEmptyPrimaryAlias(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ts := memorytopo.NewServer(ctx, "test_cell")
	tablet := &topodatapb.Tablet{
		Alias: &topodatapb.TabletAlias{
			Cell: "test_cell",
			Uid:  1,
		},
		Keyspace: "test_keyspace",
		Shard:    "0",
		Type:     topodatapb.TabletType_REPLICA,
	}
	// This will create the Keyspace, Shard and Tablet record.
	// Since this is a replica tablet, the Shard will have no primary.
	err := ts.InitTablet(ctx, tablet, false /*allowPrimaryOverride*/, true /*createShardAndKeyspace*/, false /*allowUpdate*/)
	require.NoError(t, err)

	executor := NewTabletExecutor("TestTabletExecutorOpenWithEmptyPrimaryAlias", ts, newFakeTabletManagerClient(), logutil.NewConsoleLogger(), testWaitReplicasTimeout, 0, sqlparser.NewTestParser())
	err = executor.Open(ctx, "test_keyspace")
	require.ErrorContains(t, err, "does not have a primary")
	executor.Close()
}

func TestTabletExecutorValidate(t *testing.T) {
	fakeTmc := newFakeTabletManagerClient()

	fakeTmc.AddSchemaDefinition("vt_test_keyspace", &tabletmanagerdatapb.SchemaDefinition{
		DatabaseSchema: "CREATE DATABASE `{{.DatabaseName}}` /*!40100 DEFAULT CHARACTER SET utf8 */",
		TableDefinitions: []*tabletmanagerdatapb.TableDefinition{
			{
				Name:   "test_table",
				Schema: "table schema",
				Type:   tmutils.TableBaseTable,
			},
			{
				Name:     "test_table_03",
				Schema:   "table schema",
				Type:     tmutils.TableBaseTable,
				RowCount: 200000,
			},
			{
				Name:     "test_table_04",
				Schema:   "table schema",
				Type:     tmutils.TableBaseTable,
				RowCount: 3000000,
			},
		},
	})

	executor := NewTabletExecutor("TestTabletExecutorValidate", newFakeTopo(t), fakeTmc, logutil.NewConsoleLogger(), testWaitReplicasTimeout, 0, sqlparser.NewTestParser())
	ctx := context.Background()

	sqls := []string{
		"ALTER TABLE test_table ADD COLUMN new_id bigint(20)",
		"CREATE TABLE test_table_02 (pk int)",
		"ALTER DATABASE db_name DEFAULT CHARACTER SET = utf8mb4",
		"ALTER SCHEMA db_name CHARACTER SET = utf8mb4",
	}

	err := executor.Validate(ctx, sqls)
	require.Error(t, err, "validate should fail because executor is closed")

	executor.Open(ctx, "test_keyspace")
	defer executor.Close()

	// schema changes with DMLs should fail
	err = executor.Validate(ctx, []string{
		"INSERT INTO test_table VALUES(1)",
	})
	require.Error(t, err, "schema changes are for DDLs")

	// validates valid ddls
	err = executor.Validate(ctx, sqls)
	require.NoError(t, err)

	// alter a table with more than 100,000 rows
	err = executor.Validate(ctx, []string{
		"ALTER TABLE test_table_03 ADD COLUMN new_id bigint(20)",
	})
	require.NoError(t, err, "executor.Validate should not fail, even for a table with more than 100,000 rows")

	err = executor.Validate(ctx, []string{
		"TRUNCATE TABLE test_table_04",
	})
	require.NoError(t, err, "executor.Validate should succeed, truncate a table with more than 2,000,000 rows is allowed")

	err = executor.Validate(ctx, []string{
		"DROP TABLE test_table_04",
	})
	require.NoError(t, err, "executor.Validate should succeed, drop a table with more than 2,000,000 rows is allowed")
}

func TestTabletExecutorDML(t *testing.T) {
	fakeTmc := newFakeTabletManagerClient()

	fakeTmc.AddSchemaDefinition("vt_test_keyspace", &tabletmanagerdatapb.SchemaDefinition{
		DatabaseSchema: "CREATE DATABASE `{{.DatabaseName}}` /*!40100 DEFAULT CHARACTER SET utf8 */",
		TableDefinitions: []*tabletmanagerdatapb.TableDefinition{
			{
				Name:   "test_table",
				Schema: "table schema",
				Type:   tmutils.TableBaseTable,
			},
			{
				Name:     "test_table_03",
				Schema:   "table schema",
				Type:     tmutils.TableBaseTable,
				RowCount: 200000,
			},
			{
				Name:     "test_table_04",
				Schema:   "table schema",
				Type:     tmutils.TableBaseTable,
				RowCount: 3000000,
			},
		},
	})

	executor := NewTabletExecutor("TestTabletExecutorDML", newFakeTopo(t), fakeTmc, logutil.NewConsoleLogger(), testWaitReplicasTimeout, 0, sqlparser.NewTestParser())
	ctx := context.Background()

	executor.Open(ctx, "unsharded_keyspace")
	defer executor.Close()

	// schema changes with DMLs should fail
	err := executor.Validate(ctx, []string{
		"INSERT INTO test_table VALUES(1)",
	})
	require.NoError(t, err, "executor.Validate should succeed, for DML to unsharded keyspace")
}

func TestTabletExecutorExecute(t *testing.T) {
	executor := newFakeExecutor(t)
	ctx := context.Background()

	sqls := []string{"DROP TABLE unknown_table"}

	result := executor.Execute(ctx, sqls)
	require.NotEmpty(t, result.ExecutorErr, "execute should fail, call execute.Open first")
}

func TestIsOnlineSchemaDDL(t *testing.T) {
	tt := []struct {
		query       string
		ddlStrategy string
		isOnlineDDL bool
		strategy    schema.DDLStrategy
		options     string
	}{
		{
			query:       "CREATE TABLE t(id int)",
			isOnlineDDL: false,
		},

		{
			query:       "ALTER TABLE t ADD COLUMN i INT",
			ddlStrategy: "online",
			isOnlineDDL: true,
			strategy:    schema.DDLStrategyOnline,
		},
		{
			query:       "ALTER TABLE t ADD COLUMN i INT",
			ddlStrategy: "vitess",
			isOnlineDDL: true,
			strategy:    schema.DDLStrategyVitess,
		},
		{
			query:       "ALTER TABLE t ADD COLUMN i INT",
			ddlStrategy: "",
			isOnlineDDL: false,
		},
		{
			query:       "ALTER TABLE t ADD COLUMN i INT",
			ddlStrategy: "vitess",
			isOnlineDDL: true,
			strategy:    schema.DDLStrategyVitess,
		},
		{
			query:       "ALTER TABLE t ADD COLUMN i INT",
			ddlStrategy: "vitess --declarative",
			isOnlineDDL: true,
			strategy:    schema.DDLStrategyVitess,
			options:     "--declarative",
		},
		{
			query:       "TRUNCATE TABLE t",
			ddlStrategy: "online",
			isOnlineDDL: false,
		},
		{
			query:       "TRUNCATE TABLE t",
			ddlStrategy: "vitess",
			isOnlineDDL: false,
		},
		{
			query:       "RENAME TABLE t to t2",
			ddlStrategy: "vitess",
			isOnlineDDL: false,
		},
	}

	parser := sqlparser.NewTestParser()
	for _, ts := range tt {
		e := &TabletExecutor{}
		err := e.SetDDLStrategy(ts.ddlStrategy)
		assert.NoError(t, err)

		stmt, err := parser.Parse(ts.query)
		assert.NoError(t, err)

		ddlStmt, ok := stmt.(sqlparser.DDLStatement)
		assert.True(t, ok)

		isOnlineDDL := e.isOnlineSchemaDDL(ddlStmt)
		assert.Equal(t, ts.isOnlineDDL, isOnlineDDL)
		if isOnlineDDL {
			assert.Equal(t, ts.strategy, e.ddlStrategySetting.Strategy)
			assert.Equal(t, ts.options, e.ddlStrategySetting.Options)
		}
	}
}

func TestBatchSQLs(t *testing.T) {
	sqls := []string{
		"create table t1(id int primary key)",
		"create table t2(id int primary key)",
		"create table t3(id int primary key)",
		"create table t4(id int primary key)",
		"create view v as select id from t",
	}
	tcases := []struct {
		batchSize  int
		expectSQLs []string
	}{
		{
			batchSize:  0,
			expectSQLs: sqls,
		},
		{
			batchSize:  1,
			expectSQLs: sqls,
		},
		{
			batchSize: 2,
			expectSQLs: []string{
				"create table t1(id int primary key);create table t2(id int primary key)",
				"create table t3(id int primary key);create table t4(id int primary key)",
				"create view v as select id from t",
			},
		},
		{
			batchSize: 3,
			expectSQLs: []string{
				"create table t1(id int primary key);create table t2(id int primary key);create table t3(id int primary key)",
				"create table t4(id int primary key);create view v as select id from t",
			},
		},
		{
			batchSize: 4,
			expectSQLs: []string{
				"create table t1(id int primary key);create table t2(id int primary key);create table t3(id int primary key);create table t4(id int primary key)",
				"create view v as select id from t",
			},
		},
		{
			batchSize: 5,
			expectSQLs: []string{
				"create table t1(id int primary key);create table t2(id int primary key);create table t3(id int primary key);create table t4(id int primary key);create view v as select id from t",
			},
		},
		{
			batchSize: 6,
			expectSQLs: []string{
				"create table t1(id int primary key);create table t2(id int primary key);create table t3(id int primary key);create table t4(id int primary key);create view v as select id from t",
			},
		},
	}
	for _, tcase := range tcases {
		t.Run(fmt.Sprintf("%d", tcase.batchSize), func(t *testing.T) {
			batchedSQLs := batchSQLs(sqls, tcase.batchSize)
			assert.Equal(t, tcase.expectSQLs, batchedSQLs)
		})
	}
}

func TestAllSQLsAreCreateQueries(t *testing.T) {
	tcases := []struct {
		name   string
		sqls   []string
		expect bool
	}{
		{
			name:   "empty",
			expect: true,
		},
		{
			name:   "single, yes",
			sqls:   []string{"create table t1 (id int primary key)"},
			expect: true,
		},
		{
			name:   "single, no",
			sqls:   []string{"alter table t1 force"},
			expect: false,
		},
		{
			name: "multi, no",
			sqls: []string{
				"create table t1 (id int primary key)",
				"alter table t1 force",
			},
			expect: false,
		},
		{
			name: "multi, no",
			sqls: []string{
				"alter table t1 force",
				"create table t1 (id int primary key)",
			},
			expect: false,
		},
		{
			name: "multi, yes",
			sqls: []string{
				"create table t1 (id int primary key)",
				"create table t2 (id int primary key)",
				"create table t3 (id int primary key)",
				"create view v1 as select id from t1",
			},
			expect: true,
		},
	}

	for _, tcase := range tcases {
		t.Run(tcase.name, func(t *testing.T) {
			result, err := allSQLsAreCreateQueries(tcase.sqls, sqlparser.NewTestParser())
			assert.NoError(t, err)
			assert.Equal(t, tcase.expect, result)
		})
	}
}

func TestApplyAllowZeroInDate(t *testing.T) {
	tcases := []struct {
		sql    string
		expect string
	}{
		{
			"create table t1(id int primary key); ",
			"create /*vt+ allowZeroInDate=true */ table t1 (\n\tid int primary key\n)",
		},
		{
			"create table t1(id int primary key)",
			"create /*vt+ allowZeroInDate=true */ table t1 (\n\tid int primary key\n)",
		},
		{
			"create table t1(id int primary key);select 1 from dual",
			"create /*vt+ allowZeroInDate=true */ table t1 (\n\tid int primary key\n);select 1 from dual",
		},
		{
			"create table t1(id int primary key); alter table t2 add column id2 int",
			"create /*vt+ allowZeroInDate=true */ table t1 (\n\tid int primary key\n);alter /*vt+ allowZeroInDate=true */ table t2 add column id2 int",
		},
		{
			"  ; ; ;;; create table t1(id int primary key); ;; alter table t2 add column id2 int ;;",
			"create /*vt+ allowZeroInDate=true */ table t1 (\n\tid int primary key\n);alter /*vt+ allowZeroInDate=true */ table t2 add column id2 int",
		},
	}
	for _, tcase := range tcases {
		t.Run(tcase.sql, func(t *testing.T) {
			result, err := applyAllowZeroInDate(tcase.sql, sqlparser.NewTestParser())
			assert.NoError(t, err)
			assert.Equal(t, tcase.expect, result)
		})
	}
}
