package mux9p

import (
	"fmt"
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
	"github.com/fhs/mux9p/internal/p9p"
)

type Kind int

const (
	SR Kind = iota // server read
	SW             // server write
	CR             // client read
	CW             // client write
)

func (k Kind) String() string {
	switch k {
	case SR:
		return "SR"
	case SW:
		return "SW"
	case CR:
		return "CR"
	case CW:
		return "CW"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

type fcallMsg struct {
	kind Kind
	f    p9p.Fcall
}

var versionLog = []fcallMsg{
	// The mutiplexer is itself a client for these.
	{kind: SR, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Tversion, Tag: 65535, Msize: 8092, Version: "9P2000"}}},
	{kind: SW, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Rversion, Tag: 65535, Msize: 8092, Version: "9P2000"}}},
}

var testLog = []fcallMsg{
	// These message are responded by the mux. They never go to the server.
	{kind: CW, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Tversion, Tag: 65535, Msize: 8092, Version: "9P2000"}}},
	{kind: CR, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Rversion, Tag: 65535, Msize: 8092, Version: "9P2000"}}},

	{kind: CW, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Tauth, Tag: 0, Afid: 0, Uname: "fhs", Aname: ""}}},
	{kind: SR, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Tauth, Tag: 0, Afid: 0, Uname: "fhs", Aname: ""}}},
	{kind: SW, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Rerror, Tag: 0, Ename: "acme: authentication not required"}}},
	{kind: CR, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Rerror, Tag: 0, Ename: "acme: authentication not required"}}},
	{kind: CW, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Tattach, Tag: 1, Fid: 1, Afid: ^uint32(0), Uname: "fhs", Aname: ""}}},
	{kind: SR, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Tattach, Tag: 1, Fid: 1, Afid: ^uint32(0), Uname: "fhs", Aname: ""}}},
	{kind: SW, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Rattach, Tag: 1, Qid: plan9.Qid{Path: 0, Vers: 0, Type: plan9.QTDIR}}}},
	{kind: CR, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Rattach, Tag: 1, Qid: plan9.Qid{Path: 0, Vers: 0, Type: plan9.QTDIR}}}},
	{kind: CW, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Twalk, Tag: 2, Fid: 1, Newfid: 2, Wname: []string{"ctl"}}}},
	{kind: SR, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Twalk, Tag: 2, Fid: 1, Newfid: 2, Wname: []string{"ctl"}}}},
	{kind: SW, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Rwalk, Tag: 2, Wqid: []plan9.Qid{{Path: 1, Vers: 0, Type: 0}}}}},
	{kind: CR, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Rwalk, Tag: 2, Wqid: []plan9.Qid{{Path: 1, Vers: 0, Type: 0}}}}},

	// Server sees a Topenfd as Topen
	{kind: CW, f: p9p.Fcall{Fcall: plan9.Fcall{Type: p9p.Topenfd, Tag: 2, Fid: 2, Mode: plan9.OREAD}}},
	{kind: SR, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Topen, Tag: 2, Fid: 2, Mode: plan9.OREAD}}},
	{kind: SW, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Ropen, Tag: 2, Qid: plan9.Qid{Path: 1, Vers: 0, Type: plan9.QTDIR}, Iounit: 1024}}},
	{kind: CR, f: p9p.Fcall{Fcall: plan9.Fcall{Type: p9p.Ropenfd, Tag: 2, Qid: plan9.Qid{Path: 1, Vers: 0, Type: plan9.QTDIR}, Iounit: 1024}, Unixfd: 3}},

	// mux will read for client for the openfd pipe. Server sends EOF and mux clunks the fid.
	{kind: SR, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Tread, Tag: 2, Fid: 2, Offset: 0, Count: 8068}}},
	{kind: SW, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Rread, Tag: 2, Count: 0, Data: nil}}},
	{kind: SR, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Tclunk, Tag: 2, Fid: 2}}},
	{kind: SW, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Rclunk, Tag: 2}}},

	{kind: CW, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Tclunk, Tag: 0, Fid: 1}}},
	{kind: SR, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Tclunk, Tag: 0, Fid: 1}}},
	{kind: SW, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Rclunk, Tag: 0}}},
	{kind: CR, f: p9p.Fcall{Fcall: plan9.Fcall{Type: plan9.Rclunk, Tag: 0}}},
}

func replayLog(t *testing.T, l []fcallMsg, srv io.ReadWriter, cli io.ReadWriter) {
	srvTag := make(map[uint16]uint16)
	srvFid := make(map[uint32]uint32)

	for _, m := range l {
		t.Logf("%v: Fcall = %v", m.kind, m.f.String())
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
			f, err := p9p.ReadFcall(r)
			if err != nil {
				t.Fatalf("%v: ReadFcall failed: %v", m.kind, err)
			}
			if m.kind == SR && m.f.Tag != f.Tag {
				srvTag[m.f.Tag] = f.Tag
				m.f.Tag = f.Tag // ignore Tag changes
			}
			if m.kind == SR && m.f.Fid != f.Fid {
				srvFid[m.f.Fid] = f.Fid
				m.f.Fid = f.Fid // ignore Fid changes
			}
			if m.kind == SR && m.f.Newfid != f.Newfid {
				srvFid[m.f.Newfid] = f.Newfid
				m.f.Newfid = f.Newfid // ignore Newfid changes
			}
			if m.kind == CR && m.f.Type == p9p.Ropenfd {
				m.f.Unixfd = f.Unixfd // ignore Unixfd
				_, err := p9p.ReceiveFD(r.(*net.UnixConn))
				if err != nil {
					t.Errorf("%v: ReceiveFD: %v", m.kind, err)
				}
			}
			if !reflect.DeepEqual(&m.f, f) {
				t.Errorf("%v: Fcall mismatch:\nwant: %v\n got: %v", m.kind, &m.f, f)
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
		LogLevel: 0,
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
