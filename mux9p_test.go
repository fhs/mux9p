package mux9p

import (
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"9fans.net/go/plan9"
)

const (
	SR = iota // server read
	SW        // server write
	CR        // client read
	CW        // client write
)

type fcallMsg struct {
	kind int
	f    plan9.Fcall
}

var versionLog = []fcallMsg{
	// The mutiplexer is itself a client for these.
	{SR, plan9.Fcall{Type: plan9.Tversion, Tag: 65535, Msize: 8092, Version: "9P2000"}},
	{SW, plan9.Fcall{Type: plan9.Rversion, Tag: 65535, Msize: 8092, Version: "9P2000"}},
}

var testLog = []fcallMsg{
	// These message are responded by the mux. They never go to the server.
	{CW, plan9.Fcall{Type: plan9.Tversion, Tag: 65535, Msize: 8092, Version: "9P2000"}},
	{CR, plan9.Fcall{Type: plan9.Rversion, Tag: 65535, Msize: 8092, Version: "9P2000"}},

	{CW, plan9.Fcall{Type: plan9.Tauth, Tag: 0, Afid: 0, Uname: "fhs", Aname: ""}},
	{SR, plan9.Fcall{Type: plan9.Tauth, Tag: 0, Afid: 0, Uname: "fhs", Aname: ""}},
	{SW, plan9.Fcall{Type: plan9.Rerror, Tag: 0, Ename: "acme: authentication not required"}},
	{CR, plan9.Fcall{Type: plan9.Rerror, Tag: 0, Ename: "acme: authentication not required"}},
	{CW, plan9.Fcall{Type: plan9.Tattach, Tag: 1, Fid: 1, Afid: ^uint32(0), Uname: "fhs", Aname: ""}},
	{SR, plan9.Fcall{Type: plan9.Tattach, Tag: 1, Fid: 1, Afid: ^uint32(0), Uname: "fhs", Aname: ""}},
	{SW, plan9.Fcall{Type: plan9.Rattach, Tag: 1, Qid: plan9.Qid{0, 0, plan9.QTDIR}}},
	{CR, plan9.Fcall{Type: plan9.Rattach, Tag: 1, Qid: plan9.Qid{0, 0, plan9.QTDIR}}},
	{CW, plan9.Fcall{Type: plan9.Tclunk, Tag: 0, Fid: 1}},
	{SR, plan9.Fcall{Type: plan9.Tclunk, Tag: 0, Fid: 1}},
	{SW, plan9.Fcall{Type: plan9.Rclunk, Tag: 0}},
	{CR, plan9.Fcall{Type: plan9.Rclunk, Tag: 0}},
}

func replayLog(t *testing.T, l []fcallMsg, srv io.ReadWriter, cli io.ReadWriter) {
	srvTag := make(map[uint16]uint16, 0)
	srvFid := make(map[uint32]uint32, 0)

	for _, m := range l {
		var (
			r io.Reader
			w io.Writer
		)
		switch m.kind {
		case SR:
			r = srv
		case SW:
			w = srv
		case CR:
			r = cli
		case CW:
			w = cli
		}
		if r != nil {
			f, err := plan9.ReadFcall(r)
			if err != nil {
				t.Fatalf("ReadFcall failed: %v", err)
			}
			if m.kind == SR && m.f.Tag != f.Tag {
				srvTag[m.f.Tag] = f.Tag
				m.f.Tag = f.Tag // ignore Tag changes
			}
			if m.kind == SR && m.f.Fid != f.Fid {
				srvFid[m.f.Fid] = f.Fid
				m.f.Fid = f.Fid // ignore Fid changes
			}
			if !reflect.DeepEqual(&m.f, f) {
				t.Errorf("Fcall mismatch:\nwant: %v\n got: %v", &m.f, f)
			}
		}
		if w != nil {
			if m.kind == SW {
				tag, ok := srvTag[m.f.Tag]
				if ok {
					m.f.Tag = tag
				}

				fid, ok := srvFid[m.f.Fid]
				if ok {
					m.f.Fid = fid
				}
			}
			b, err := m.f.Bytes()
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
	srv := &readWriter{rx, tx}
	muxSrv := &readWriter{r, w}

	network := "unix"
	address := filepath.Join(dir, "testsrv.sock")
	go Listen(network, address, muxSrv, &Config{
		LogLevel: 2,
	})

	replayLog(t, versionLog, srv, nil)

	cli, err := net.DialTimeout(network, address, 5*time.Second)
	if err != nil {
		t.Fatalf("Dial failed: %v\n", err)
	}
	defer cli.Close()

	replayLog(t, testLog, srv, cli)
}

type readWriter struct {
	r io.Reader
	w io.Writer
}

func (rw *readWriter) Read(b []byte) (int, error) { return rw.r.Read(b) }

func (rw *readWriter) Write(b []byte) (int, error) { return rw.w.Write(b) }
