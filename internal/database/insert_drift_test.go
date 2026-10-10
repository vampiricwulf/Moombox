package database

import (
	"reflect"
	"slices"
	"testing"
)

// runtimeOnlyJobFields are the Job fields AddJob deliberately does not write:
// state a download, the player or a later path fills in, plus the fields that
// are not columns at all. Everything else a creator sets must survive the
// insert.
var runtimeOnlyJobFields = []string{
	// Not columns, or not stored by AddJob's row insert.
	"Version", "Gaps", "Trims", "Segments", "UpdatedAt",
	// Download progress.
	"LastVideoSeq", "LastAudioSeq", "TotalVideoSeq", "TotalAudioSeq",
	"VideoWidth", "VideoHeight", "VideoFps", "IncompleteTail", "AutoRetryCount",
	// Player and watch state.
	"Watched", "ResumePosition", "ChatOffset",
	// Parking and notification bookkeeping, written by their own paths.
	"ParkReason", "ParkIdentity", "NotificationMsgs",
}

// AddJob's INSERT names a fixed column list, so a Job field outside it takes
// the schema default whatever the caller set — silently: the archive import
// set file_size on its new row, the INSERT dropped it, and the import route
// (which answers with the struct it built) reported a size the database did
// not hold. Every field is set non-zero here and must read back, unless it is
// on runtimeOnlyJobFields — so a new column has to be added to the insert or
// listed there on purpose.
//
// Mutant: dropping file_size (or any other column) from insertJobExec.
func TestAddJobStoresEveryCreationField(t *testing.T) {
	db := newTestDB(t)
	in := &Job{ID: "drift", VideoID: "drift", Status: StatusFinished}
	v := reflect.ValueOf(in).Elem()
	tp := v.Type()
	for i := range tp.NumField() {
		name := tp.Field(i).Name
		if slices.Contains(runtimeOnlyJobFields, name) || name == "ID" || name == "VideoID" || name == "Status" {
			continue
		}
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("v-" + name)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int, reflect.Int64:
			f.SetInt(7)
		case reflect.Float64:
			f.SetFloat(7.5)
		case reflect.Pointer:
			p := reflect.New(f.Type().Elem())
			switch p.Elem().Kind() {
			case reflect.String:
				p.Elem().SetString("v-" + name)
			case reflect.Int, reflect.Int64:
				p.Elem().SetInt(7)
			case reflect.Float64:
				p.Elem().SetFloat(7.5)
			default:
				t.Fatalf("field %s: unhandled pointer kind %s — extend this test", name, p.Elem().Kind())
			}
			f.Set(p)
		default:
			t.Fatalf("field %s: unhandled kind %s — extend this test or list it as runtime-only", name, f.Kind())
		}
	}
	if added, err := db.AddJob(in); err != nil || !added {
		t.Fatalf("AddJob = %v, %v", added, err)
	}
	out, err := db.GetJob("drift")
	if err != nil || out == nil {
		t.Fatalf("GetJob: %v", err)
	}
	ov := reflect.ValueOf(out).Elem()
	for i := range tp.NumField() {
		name := tp.Field(i).Name
		if slices.Contains(runtimeOnlyJobFields, name) {
			continue
		}
		if !reflect.DeepEqual(v.Field(i).Interface(), ov.Field(i).Interface()) {
			t.Errorf("%s: set %v, AddJob stored %v — add its column to insertJobExec or list it in runtimeOnlyJobFields",
				name, deref(v.Field(i)), deref(ov.Field(i)))
		}
	}
}

func deref(f reflect.Value) any {
	if f.Kind() == reflect.Pointer && !f.IsNil() {
		return f.Elem().Interface()
	}
	return f.Interface()
}
