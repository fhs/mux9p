package mux9p

import (
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"9fans.net/go/plan9"
)

var versionLog = []plan9.Fcall{
	plan9.Fcall{Type: plan9.Tversion, Tag: 65535, Msize: 8092, Version: "9P2000"},
	plan9.Fcall{Type: plan9.Rversion, Tag: 65535, Msize: 8092, Version: "9P2000"},
}

var testServerLog = []plan9.Fcall{
	plan9.Fcall{Type: plan9.Tauth, Tag: 0, Afid: 0, Uname: "fhs", Aname: ""},
	plan9.Fcall{Type: plan9.Rerror, Tag: 0, Ename: "acme: authentication not required"},
}

var testClientLog = []plan9.Fcall{
	plan9.Fcall{Type: plan9.Tauth, Tag: 0, Afid: 0, Uname: "fhs", Aname: ""},
	plan9.Fcall{Type: plan9.Rerror, Tag: 0, Ename: "acme: authentication not required"},
}

func replayLog(t *testing.T, l []plan9.Fcall, r io.Reader, w io.Writer, isClient bool) {
	for _, f := range l {
		log.Printf("replaying %v: %v\n", isClient, &f)
		if xor(f.Type%2 == 0, isClient) {
			f1, err := plan9.ReadFcall(r)
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
			_, err = w.Write(b)
			if err != nil {
				t.Fatalf("writing Fcall failed: %v", err)
			}
		}
	}
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
	defer rx.Close()
	r, tx := io.Pipe()
	defer tx.Close()

	network := "unix"
	address := filepath.Join(dir, "testsrv.sock")
	go Listen(network, address, &Config{
		Reader: r,
		Writer: w,
	})

	replayLog(t, versionLog, rx, tx, false)

	go func() {
		c, err := net.DialTimeout(network, address, 5*time.Second)
		if err != nil {
			t.Fatalf("Dial failed: %v\n", err)
		}
		defer c.Close()
		replayLog(t, testClientLog, c, c, true)
	}()

	replayLog(t, testServerLog, rx, tx, false)
}

func xor(a, b bool) bool {
	return (a || b) && !(a && b)
}
