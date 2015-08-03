package bz1_test

import (
	"bytes"
	"encoding/binary"
	"io/ioutil"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/influxdb/influxdb/tsdb"
	"github.com/influxdb/influxdb/tsdb/engine/bz1"
)

// Ensure the engine can write series metadata and reload it.
func TestEngine_LoadMetadataIndex_Series(t *testing.T) {
	e := OpenDefaultEngine()
	defer e.Close()

	// Setup nop mock.
	e.PointsWriter.WritePointsFn = func(a []tsdb.Point) error { return nil }

	// Write series metadata.
	if err := e.WritePoints(nil, nil, []*tsdb.SeriesCreate{
		{Series: &tsdb.Series{Key: string(tsdb.MakeKey([]byte("cpu"), map[string]string{"host": "server0"})), Tags: map[string]string{"host": "server0"}}},
		{Series: &tsdb.Series{Key: string(tsdb.MakeKey([]byte("cpu"), map[string]string{"host": "server1"})), Tags: map[string]string{"host": "server1"}}},
		{Series: &tsdb.Series{Key: "series with spaces"}},
	}); err != nil {
		t.Fatal(err)
	}

	// Load metadata index.
	index := tsdb.NewDatabaseIndex()
	if err := e.LoadMetadataIndex(index, make(map[string]*tsdb.MeasurementFields)); err != nil {
		t.Fatal(err)
	}

	// Verify index is correct.
	if m := index.Measurement("cpu"); m == nil {
		t.Fatal("measurement not found")
	} else if s := m.SeriesByID(1); s.Key != "cpu,host=server0" || !reflect.DeepEqual(s.Tags, map[string]string{"host": "server0"}) {
		t.Fatalf("unexpected series: %q / %#v", s.Key, s.Tags)
	} else if s = m.SeriesByID(2); s.Key != "cpu,host=server1" || !reflect.DeepEqual(s.Tags, map[string]string{"host": "server1"}) {
		t.Fatalf("unexpected series: %q / %#v", s.Key, s.Tags)
	}

	if m := index.Measurement("series with spaces"); m == nil {
		t.Fatal("measurement not found")
	} else if s := m.SeriesByID(3); s.Key != "series with spaces" {
		t.Fatalf("unexpected series: %q", s.Key)
	}
}

// Ensure the engine can write field metadata and reload it.
func TestEngine_LoadMetadataIndex_Fields(t *testing.T) {
	e := OpenDefaultEngine()
	defer e.Close()

	// Setup nop mock.
	e.PointsWriter.WritePointsFn = func(a []tsdb.Point) error { return nil }

	// Write series metadata.
	if err := e.WritePoints(nil, map[string]*tsdb.MeasurementFields{
		"cpu": &tsdb.MeasurementFields{
			Fields: map[string]*tsdb.Field{
				"value": &tsdb.Field{ID: 0, Name: "value"},
			},
		},
	}, nil); err != nil {
		t.Fatal(err)
	}

	// Load metadata index.
	mfs := make(map[string]*tsdb.MeasurementFields)
	if err := e.LoadMetadataIndex(tsdb.NewDatabaseIndex(), mfs); err != nil {
		t.Fatal(err)
	}

	// Verify measurement field is correct.
	if mf := mfs["cpu"]; mf == nil {
		t.Fatal("measurement fields not found")
	} else if !reflect.DeepEqual(mf.Fields, map[string]*tsdb.Field{"value": &tsdb.Field{ID: 0, Name: "value"}}) {
		t.Fatalf("unexpected fields: %#v", mf.Fields)
	}
}

// Ensure the engine can write points to storage.
func TestEngine_WritePoints_PointsWriter(t *testing.T) {
	e := OpenDefaultEngine()
	defer e.Close()

	// Points to be inserted.
	points := []tsdb.Point{
		tsdb.NewPoint("cpu", tsdb.Tags{}, tsdb.Fields{}, time.Unix(0, 1)),
		tsdb.NewPoint("cpu", tsdb.Tags{}, tsdb.Fields{}, time.Unix(0, 0)),
		tsdb.NewPoint("cpu", tsdb.Tags{}, tsdb.Fields{}, time.Unix(1, 0)),

		tsdb.NewPoint("cpu", tsdb.Tags{"host": "serverA"}, tsdb.Fields{}, time.Unix(0, 0)),
	}

	// Mock points writer to ensure points are passed through.
	var invoked bool
	e.PointsWriter.WritePointsFn = func(a []tsdb.Point) error {
		invoked = true
		if !reflect.DeepEqual(points, a) {
			t.Fatalf("unexpected points: %#v", a)
		}
		return nil
	}

	// Write points against two separate series.
	if err := e.WritePoints(points, nil, nil); err != nil {
		t.Fatal(err)
	} else if !invoked {
		t.Fatal("PointsWriter.WritePoints() not called")
	}
}

// Ensure the engine can write points to the index.
func TestEngine_WriteIndex_Append(t *testing.T) {
	e := OpenDefaultEngine()
	defer e.Close()

	// Append points to index.
	if err := e.WriteIndex(map[string][][]byte{
		"cpu": [][]byte{
			bz1.MarshalEntry(1, []byte{0x10}),
			bz1.MarshalEntry(2, []byte{0x20}),
		},
		"mem": [][]byte{
			bz1.MarshalEntry(0, []byte{0x30}),
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Start transaction.
	tx := e.MustBegin(false)
	defer tx.Rollback()

	// Iterate over "cpu" series.
	c := tx.Cursor("cpu")
	if k, v := c.Seek(u64tob(0)); !reflect.DeepEqual(k, []byte{0, 0, 0, 0, 0, 0, 0, 1}) || !reflect.DeepEqual(v, []byte{0x10}) {
		t.Fatalf("unexpected key/value: %x / %x", k, v)
	} else if k, v = c.Next(); !reflect.DeepEqual(k, []byte{0, 0, 0, 0, 0, 0, 0, 2}) || !reflect.DeepEqual(v, []byte{0x20}) {
		t.Fatalf("unexpected key/value: %x / %x", k, v)
	} else if k, _ = c.Next(); k != nil {
		t.Fatalf("unexpected key/value: %x / %x", k, v)
	}

	// Iterate over "mem" series.
	c = tx.Cursor("mem")
	if k, v := c.Seek(u64tob(0)); !reflect.DeepEqual(k, []byte{0, 0, 0, 0, 0, 0, 0, 0}) || !reflect.DeepEqual(v, []byte{0x30}) {
		t.Fatalf("unexpected key/value: %x / %x", k, v)
	} else if k, _ = c.Next(); k != nil {
		t.Fatalf("unexpected key/value: %x / %x", k, v)
	}
}

// Ensure the engine can rewrite blocks that contain the new point range.
func TestEngine_WriteIndex_Insert(t *testing.T) {
	e := OpenDefaultEngine()
	defer e.Close()

	// Write initial points to index.
	if err := e.WriteIndex(map[string][][]byte{
		"cpu": [][]byte{
			bz1.MarshalEntry(10, []byte{0x10}),
			bz1.MarshalEntry(20, []byte{0x20}),
			bz1.MarshalEntry(30, []byte{0x30}),
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Write overlapping points to index.
	if err := e.WriteIndex(map[string][][]byte{
		"cpu": [][]byte{
			bz1.MarshalEntry(9, []byte{0x09}),
			bz1.MarshalEntry(10, []byte{0xFF}),
			bz1.MarshalEntry(25, []byte{0x25}),
			bz1.MarshalEntry(31, []byte{0x31}),
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Write overlapping points to index again.
	if err := e.WriteIndex(map[string][][]byte{
		"cpu": [][]byte{
			bz1.MarshalEntry(31, []byte{0xFF}),
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Start transaction.
	tx := e.MustBegin(false)
	defer tx.Rollback()

	// Iterate over "cpu" series.
	c := tx.Cursor("cpu")
	if k, v := c.Seek(u64tob(0)); btou64(k) != 9 || !bytes.Equal(v, []byte{0x09}) {
		t.Fatalf("unexpected key/value: %x / %x", k, v)
	} else if k, v = c.Next(); btou64(k) != 10 || !bytes.Equal(v, []byte{0xFF}) {
		t.Fatalf("unexpected key/value: %x / %x", k, v)
	} else if k, v = c.Next(); btou64(k) != 20 || !bytes.Equal(v, []byte{0x20}) {
		t.Fatalf("unexpected key/value: %x / %x", k, v)
	} else if k, v = c.Next(); btou64(k) != 25 || !bytes.Equal(v, []byte{0x25}) {
		t.Fatalf("unexpected key/value: %x / %x", k, v)
	} else if k, v = c.Next(); btou64(k) != 30 || !bytes.Equal(v, []byte{0x30}) {
		t.Fatalf("unexpected key/value: %x / %x", k, v)
	} else if k, v = c.Next(); btou64(k) != 31 || !bytes.Equal(v, []byte{0xFF}) {
		t.Fatalf("unexpected key/value: %x / %x", k, v)
	}
}

// Engine represents a test wrapper for bz1.Engine.
type Engine struct {
	*bz1.Engine
	PointsWriter EnginePointsWriter
}

// NewEngine returns a new instance of Engine.
func NewEngine(opt tsdb.EngineOptions) *Engine {
	// Generate temporary file.
	f, _ := ioutil.TempFile("", "bz1-")
	f.Close()
	os.Remove(f.Name())

	// Create test wrapper and attach mocks.
	e := &Engine{
		Engine: bz1.NewEngine(f.Name(), opt).(*bz1.Engine),
	}
	e.Engine.PointsWriter = &e.PointsWriter
	return e
}

// OpenEngine returns an opened instance of Engine. Panic on error.
func OpenEngine(opt tsdb.EngineOptions) *Engine {
	e := NewEngine(opt)
	if err := e.Open(); err != nil {
		panic(err)
	}
	return e
}

// OpenDefaultEngine returns an open Engine with default options.
func OpenDefaultEngine() *Engine { return OpenEngine(tsdb.NewEngineOptions()) }

// Close closes the engine and removes all data.
func (e *Engine) Close() error {
	e.Engine.Close()
	os.RemoveAll(e.Path())
	return nil
}

// MustBegin returns a new tranaction. Panic on error.
func (e *Engine) MustBegin(writable bool) tsdb.Tx {
	tx, err := e.Begin(writable)
	if err != nil {
		panic(err)
	}
	return tx
}

// EnginePointsWriter represents a mock that implements Engine.PointsWriter.
type EnginePointsWriter struct {
	WritePointsFn func(points []tsdb.Point) error
}

func (w *EnginePointsWriter) WritePoints(points []tsdb.Point) error {
	return w.WritePointsFn(points)
}

// copyBytes returns a copy of a byte slice.
func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}

	other := make([]byte, len(b))
	copy(other, b)
	return other
}

// u64tob converts a uint64 into an 8-byte slice.
func u64tob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// btou64 converts an 8-byte slice into an uint64.
func btou64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }
