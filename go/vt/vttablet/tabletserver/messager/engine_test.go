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

package messager

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"vitess.io/vitess/go/sqltypes"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vtenv"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/schema"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/tabletenv"
)

var (
	meTableT1 = &schema.Table{
		Name:        sqlparser.NewIdentifierCS("t1"),
		Type:        schema.Message,
		MessageInfo: newMMTable().MessageInfo,
	}
	meTableT2 = &schema.Table{
		Name:        sqlparser.NewIdentifierCS("t2"),
		Type:        schema.Message,
		MessageInfo: newMMTable().MessageInfo,
	}
	meTableT3 = &schema.Table{
		Name:        sqlparser.NewIdentifierCS("t3"),
		Type:        schema.Message,
		MessageInfo: newMMTable().MessageInfo,
	}
	meTableT4 = &schema.Table{
		Name:        sqlparser.NewIdentifierCS("t4"),
		Type:        schema.Message,
		MessageInfo: newMMTable().MessageInfo,
	}

	tableT2 = &schema.Table{
		Name: sqlparser.NewIdentifierCS("t2"),
		Type: schema.NoType,
	}
	tableT4 = &schema.Table{
		Name: sqlparser.NewIdentifierCS("t4"),
		Type: schema.NoType,
	}
	tableT5 = &schema.Table{
		Name: sqlparser.NewIdentifierCS("t5"),
		Type: schema.NoType,
	}
)

func TestEngineSchemaChanged(t *testing.T) {
	engine := newTestEngine()
	defer engine.Close()

	engine.schemaChanged(nil, []*schema.Table{meTableT1, tableT2}, nil, nil, true)
	got := extractManagerNames(engine.managers)
	want := map[string]bool{"t1": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got: %+v, want %+v", got, want)
	}

	engine.schemaChanged(nil, []*schema.Table{meTableT3}, nil, nil, true)
	got = extractManagerNames(engine.managers)
	want = map[string]bool{"t1": true, "t3": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got: %+v, want %+v", got, want)
	}

	engine.schemaChanged(nil, []*schema.Table{meTableT4}, nil, []*schema.Table{meTableT3, tableT5}, true)
	got = extractManagerNames(engine.managers)
	want = map[string]bool{"t1": true, "t4": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got: %+v, want %+v", got, want)
	}
	// Test update
	engine.schemaChanged(nil, nil, []*schema.Table{meTableT2, tableT4}, nil, true)
	got = extractManagerNames(engine.managers)
	want = map[string]bool{"t1": true, "t2": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got: %+v, want %+v", got, want)
	}
}

func extractManagerNames(in map[string]*messageManager) map[string]bool {
	out := make(map[string]bool)
	for k := range in {
		out[k] = true
	}
	return out
}

func TestSubscribe(t *testing.T) {
	engine := newTestEngine()
	engine.schemaChanged(nil, []*schema.Table{meTableT1, meTableT2}, nil, nil, true)
	f1, ch1 := newEngineReceiver()
	f2, ch2 := newEngineReceiver()
	// Each receiver is subscribed to different managers.
	engine.Subscribe(context.Background(), "t1", f1)
	<-ch1
	engine.Subscribe(context.Background(), "t2", f2)
	<-ch2
	engine.managers["t1"].Add(&MessageRow{Row: []sqltypes.Value{sqltypes.NewVarBinary("1")}})
	engine.managers["t2"].Add(&MessageRow{Row: []sqltypes.Value{sqltypes.NewVarBinary("2")}})
	<-ch1
	<-ch2

	// Error case.
	want := "message table t3 not found"
	_, err := engine.Subscribe(context.Background(), "t3", f1)
	if err == nil || err.Error() != want {
		t.Errorf("Subscribe: %v, want %s", err, want)
	}

	// After close, Subscribe should return a closed channel.
	engine.Close()
	_, err = engine.Subscribe(context.Background(), "t1", nil)
	if got, want := vterrors.Code(err), vtrpcpb.Code_UNAVAILABLE; got != want {
		t.Errorf("Subscribed on closed engine error code: %v, want %v", got, want)
	}
}

func TestEngineGenerate(t *testing.T) {
	engine := newTestEngine()
	defer engine.Close()
	engine.schemaChanged(nil, []*schema.Table{meTableT1}, nil, nil, true)

	if _, err := engine.GetGenerator("t1"); err != nil {
		t.Error(err)
	}
	want := "message table t2 not found in schema"
	if _, err := engine.GetGenerator("t2"); err == nil || err.Error() != want {
		t.Errorf("engine.GenerateAckQuery(invalid): %v, want %s", err, want)
	}
}

func newTestEngine() *Engine {
	cfg := tabletenv.NewDefaultConfig()
	tsv := &fakeTabletServer{
		Env: tabletenv.NewEnv(vtenv.NewTestEnv(), cfg, "MessagerTest"),
	}
	se := schema.NewEngineForTests()
	te := NewEngine(tsv, se, newFakeVStreamer())
	te.Open()
	return te
}

func newEngineReceiver() (f func(qr *sqltypes.Result) error, ch chan *sqltypes.Result) {
	ch = make(chan *sqltypes.Result, 1)
	return func(qr *sqltypes.Result) error {
		ch <- qr
		return nil
	}, ch
}

// TestDeadlockBwCloseAndSchemaChange tests the deadlock observed between Close and schemaChanged
// functions. More details can be found in the issue https://github.com/vitessio/vitess/issues/17229.
func TestDeadlockBwCloseAndSchemaChange(t *testing.T) {
	engine := newTestEngine()
	defer engine.Close()
	se := engine.se

	wg := sync.WaitGroup{}
	wg.Add(2)
	// Try running Close and schemaChanged in parallel multiple times.
	// This reproduces the deadlock quite readily.
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			engine.Close()
			engine.Open()
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			se.BroadcastForTesting(nil, nil, nil, true)
		}
	}()

	// Wait for wait group to finish.
	wg.Wait()
}
