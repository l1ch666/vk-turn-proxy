package baseline

import (
	"reflect"
	"testing"
)

func TestIPerfArguments(t *testing.T) {
	upload, err := IPerfArguments("127.0.0.1:9000", 20, Case{Direction: DirectionUpload, Parallel: 1})
	if err != nil {
		t.Fatalf("upload args: %v", err)
	}
	wantUpload := []string{"-c", "127.0.0.1", "-p", "9000", "-J", "-t", "20", "-P", "1"}
	if !reflect.DeepEqual(upload, wantUpload) {
		t.Fatalf("upload args = %v, want %v", upload, wantUpload)
	}
	download, err := IPerfArguments("[::1]:9000", 30, Case{Direction: DirectionDownload, Parallel: 8})
	if err != nil {
		t.Fatalf("download args: %v", err)
	}
	wantDownload := []string{"-c", "::1", "-p", "9000", "-J", "-t", "30", "-P", "8", "-R"}
	if !reflect.DeepEqual(download, wantDownload) {
		t.Fatalf("download args = %v, want %v", download, wantDownload)
	}
}
