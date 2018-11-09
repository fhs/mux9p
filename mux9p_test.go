package mux9p

import (
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"testing"

	"9fans.net/go/plan9"
)

var testServerLog = []plan9.Fcall{
	plan9.Fcall{Type: plan9.Tversion, Tag: 65535, Msize: 8092, Version: "9P2000"},
	plan9.Fcall{Type: plan9.Rversion, Tag: 65535, Msize: 8092, Version: "9P2000"},
}

func TestListen(t *testing.T) {
	dir, err := ioutil.TempDir("", "mux9p")
	if err != nil {
		t.Errorf("failed to create temporary directory: %v\n", err)
	}
	defer os.RemoveAll(dir)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		os.RemoveAll(dir)
		signal.Stop(c)
	}()

	rx, w := io.Pipe()
	r, tx := io.Pipe()
	go Listen("unix", filepath.Join(dir, "testsrv.sock"), &Config{
		Reader: r,
		Writer: w,
	})

	for _, f := range testServerLog {
		if f.Type%2 == 0 {
			f1, err := plan9.ReadFcall(rx)
			if err != nil {
				t.Fatalf("ReadFcall failed: %v", err)
			}
			if !reflect.DeepEqual(&f, f1) {
				t.Errorf("Fcall mismatch:\nwant: %v\n got: %v", f, f1)
			}
		} else {
			b, err := f.Bytes()
			if err != nil {
				t.Fatalf("failed to encode Fcall: %v", err)
			}
			_, err = tx.Write(b)
			if err != nil {
				t.Fatalf("writing Fcall failed: %v", err)
			}
		}
	}
}
