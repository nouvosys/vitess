/*
Copyright 2024 The Vitess Authors.

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

package vreplication

import (
	"testing"

	"github.com/stretchr/testify/require"

	"vitess.io/vitess/go/test/endtoend/cluster"
	vttablet "vitess.io/vitess/go/vt/vttablet/common"
)

type TestClusterSpec struct {
	keyspaceName string
	vschema      string
	schema       string
}

var lookupClusterSpec = TestClusterSpec{
	keyspaceName: "lookup",
	vschema: `
{
  "sharded": true,
  "vindexes": {
    "reverse_bits": {
      "type": "reverse_bits"
    }
  },
  "tables": {
	"t1": {
	      "column_vindexes": [
	        {
	          "column": "c1",
	          "name": "reverse_bits"
	        }
	      ]
	}
  }
}
`,
	schema: `
create table t1(
	c1 int,
	c2 int,
	val varchar(128),
	primary key(c1)
);
`,
}

func setupLookupIndexKeyspace(t *testing.T) map[string]*cluster.VttabletProcess {
	tablets := make(map[string]*cluster.VttabletProcess)
	if _, err := vc.AddKeyspace(t, []*Cell{vc.Cells["zone1"]}, lookupClusterSpec.keyspaceName, "-80,80-",
		lookupClusterSpec.vschema, lookupClusterSpec.schema, defaultReplicas, defaultRdonly, 200, nil); err != nil {
		require.NoError(t, err)
	}
	defaultCell := vc.Cells[vc.CellNames[0]]
	ks := vc.Cells[defaultCell.Name].Keyspaces[lookupClusterSpec.keyspaceName]
	targetTab1 = ks.Shards["-80"].Tablets["zone1-200"].Vttablet
	targetTab2 = ks.Shards["80-"].Tablets["zone1-300"].Vttablet
	tablets["-80"] = targetTab1
	tablets["80-"] = targetTab2
	return tablets
}

type lookupTestCase struct {
	name                 string
	li                   *lookupIndex
	initQuery            string
	runningQuery         string
	postExternalizeQuery string
	cleanupQuery         string
}

func TestLookupIndex(t *testing.T) {
	setSidecarDBName("_vt")
	origDefaultReplicas := defaultReplicas
	origDefaultRdonly := defaultRdonly
	defer func() {
		defaultReplicas = origDefaultReplicas
		defaultRdonly = origDefaultRdonly
	}()
	defaultReplicas = 1
	defaultRdonly = 0
	vc = setupMinimalCluster(t)
	defer vc.TearDown()
	vttablet.InitVReplicationConfigDefaults()

	_ = setupLookupIndexKeyspace(t)

	initQuery := "insert into t1 (c1, c2, val) values (1, 1, 'val1'), (2, 2, 'val2'), (3, 3, 'val3')"
	runningQuery := "insert into t1 (c1, c2, val) values (4, 4, 'val4'), (5, 5, 'val5'), (6, 6, 'val6')"
	postExternalizeQuery := "insert into t1 (c1, c2, val) values (7, 7, 'val7'), (8, 8, 'val8'), (9, 9, 'val9')"
	cleanupQuery := "delete from t1"

	testCases := []lookupTestCase{
		{
			name: "non-unique lookup index, one column",
			li: &lookupIndex{
				typ:                "consistent_lookup",
				name:               "t1_c2_lookup",
				tableKeyspace:      lookupClusterSpec.keyspaceName,
				table:              "t1",
				columns:            []string{"c2"},
				ownerTable:         "t1",
				ownerTableKeyspace: lookupClusterSpec.keyspaceName,
				ignoreNulls:        true,
				t:                  t,
			},
		},
		{
			name: "lookup index, two columns",
			li: &lookupIndex{
				typ:                "lookup",
				name:               "t1_c2_val_lookup",
				tableKeyspace:      lookupClusterSpec.keyspaceName,
				table:              "t1",
				columns:            []string{"c2", "val"},
				ownerTable:         "t1",
				ownerTableKeyspace: lookupClusterSpec.keyspaceName,
				ignoreNulls:        true,
				t:                  t,
			},
		},
		{
			name: "unique lookup index, one column",
			li: &lookupIndex{
				typ:                "lookup_unique",
				name:               "t1_c2_unique_lookup",
				tableKeyspace:      lookupClusterSpec.keyspaceName,
				table:              "t1",
				columns:            []string{"c2"},
				ownerTable:         "t1",
				ownerTableKeyspace: lookupClusterSpec.keyspaceName,
				ignoreNulls:        true,
				t:                  t,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.initQuery = initQuery
			tc.runningQuery = runningQuery
			tc.postExternalizeQuery = postExternalizeQuery
			tc.cleanupQuery = cleanupQuery
			testLookupVindex(t, &tc)
		})
	}
}

func testLookupVindex(t *testing.T, tc *lookupTestCase) {
	vtgateConn, cancel := getVTGateConn()
	defer cancel()
	var totalRows int
	li := tc.li

	t.Run("init data", func(t *testing.T) {
		totalRows += getNumRowsInQuery(t, tc.initQuery)
		_, err := vtgateConn.ExecuteFetch(tc.initQuery, 1000, false)
		require.NoError(t, err)
	})

	t.Run("create", func(t *testing.T) {
		tc.li.create()

		lks := li.tableKeyspace
		vindexName := li.name
		waitForRowCount(t, vtgateConn, lks, vindexName, totalRows)
		totalRows += getNumRowsInQuery(t, tc.runningQuery)
		_, err := vtgateConn.ExecuteFetch(tc.runningQuery, 1000, false)
		require.NoError(t, err)
		waitForRowCount(t, vtgateConn, tc.li.ownerTableKeyspace, li.name, totalRows)
	})

	t.Run("externalize", func(t *testing.T) {
		tc.li.externalize()
		totalRows += getNumRowsInQuery(t, tc.postExternalizeQuery)
		_, err := vtgateConn.ExecuteFetch(tc.postExternalizeQuery, 1000, false)
		require.NoError(t, err)
		waitForRowCount(t, vtgateConn, tc.li.ownerTableKeyspace, li.name, totalRows)
	})

	t.Run("cleanup", func(t *testing.T) {
		_, err := vtgateConn.ExecuteFetch(tc.cleanupQuery, 1000, false)
		require.NoError(t, err)
		waitForRowCount(t, vtgateConn, tc.li.ownerTableKeyspace, li.name, 0)
	})
}