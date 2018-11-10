// This program announces and multiplexes a 9P service.
//
// It is a port of Plan 9 Port's 9pserve program.
package mux9p

// Life cycle of a 9P message:
//
// 1. conninthread:
//		read from conn into Msg.tx
//		write global tags and fids to Msg.tx
//		send Msg to Config.outq
// 2. outputthread:
//		receive from Config.outq
//		write Msg.tx to Config.Writer
// 3. inputthread:
//		read from Config.Reader into Msg.rx
//		write conn's tag to Msg.rx
//		send Msg to conn.outq
// 4. connoutthread:
//		receive from conn.outq
//		write Msg.rx to conn

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"

	"9fans.net/go/plan9"
)

type Config struct {
	NoAuth  bool
	Logging bool
	Reader  io.Reader
	Writer  io.Writer

	outq      *Queue // Msg queue
	msize     uint32
	versioned bool

	fidtab  []*Fid
	freefid *Fid

	msgtab  []*Msg
	nmsg    int
	freemsg *Msg
}

const maxMsgPerConn = 64

type Fid struct {
	fid  uint32
	cfid uint32 // Conn's fid
	ref  int    // ref counting for freefid
	next *Fid   // next in freefid
}

type Msg struct {
	c        *conn
	internal bool        // Tflush or Tclunk used for clean up
	sync     bool        // used to signal outputthread we're done
	ctag     uint16      // Conn's tag
	tag      uint16      // unique tag over all Conns
	tx       plan9.Fcall // transmit
	rx       plan9.Fcall // receive
	fid      *Fid        // Tattach, Twalk, etc.
	newfid   *Fid        // Twalk Newfid
	afid     *Fid        // Tauth Fid
	oldm     *Msg        // Msg corresponding to Tflush Oldtag
	ref      int         // ref counting for freemsg
	next     *Msg        // next in freemsg
}

type conn struct {
	conn         net.Conn
	nmsg         int             // number of outstanding messages
	inc          chan struct{}   // continue if inputstalled
	internal     chan *Msg       // used to send internal Msgs
	inputstalled bool            // too many messages being processed
	tag          map[uint16]*Msg // conn tag → global tag
	fid          map[uint32]*Fid // conn fid → global fid
	outq         *Queue          // Msg queue
	outqdead     chan struct{}   // done using outq or Conn.outq
}

var (
	verbose = 2 // maybe make this part of Config later
)

func Listen(network, address string, cfg *Config) {
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.Reader == nil {
		cfg.Reader = os.Stdin
	}
	if cfg.Writer == nil {
		cfg.Writer = os.Stdout
	}
	x := os.Getenv("verbose9pserve")
	if x != "" {
		var err error
		verbose, err = strconv.Atoi(x)
		if err != nil {
			verbose = 0
		}
		fmt.Fprintf(os.Stderr, "verbose9pserve %s => %d\n", x, verbose)
	}

	ln, err := net.Listen(network, address)
	if err != nil {
		log.Fatalf("listen failed: %v\n", err)
	}
	defer ln.Close()

	if cfg.Logging {
		f, err := os.Create(fmt.Sprintf("%s.log", address))
		if err != nil {
			log.Fatalf("create failed: %v", err)
		}
		defer f.Close()
		log.SetOutput(f)
	}

	cfg.outq = newQueue()
	cfg.msize = 8092
	cfg.mainproc(ln)
}

func (cfg *Config) mainproc(ln net.Listener) {
	vprintf("9pserve running\n")
	//atnotify(ignorepipe, 1)

	if !cfg.versioned {
		f := &plan9.Fcall{
			Type:    plan9.Tversion,
			Version: "9P2000",
			Msize:   cfg.msize,
			Tag:     plan9.NOTAG,
		}
		vbuf, err := f.Bytes()
		if err != nil {
			log.Fatalf("Fcall conversion to bytes failed: %v", err)
		}
		vvprintf("* <- %v\n", f)
		_, err = cfg.Writer.Write(vbuf)
		if err != nil {
			log.Fatalf("error writing Tversion: %v", err)
		}
		f, err = plan9.ReadFcall(cfg.Reader)
		if err != nil {
			log.Fatalf("ReadFcall failed: %v", err)
		}
		if f.Msize < cfg.msize {
			cfg.msize = f.Msize
		}
		vvprintf("* -> %v\n", f)
	}

	go cfg.inputthread()
	go cfg.outputthread()

	cfg.listenthread(ln)
}

func (cfg *Config) listenthread(ln net.Listener) {
	for {
		var c conn
		var err error
		c.conn, err = ln.Accept()
		if err != nil {
			vprintf("listen: %v\n", err)
			return
		}
		c.inc = make(chan struct{})
		c.internal = make(chan *Msg)
		c.outq = newQueue()
		c.outqdead = make(chan struct{})
		c.tag = make(map[uint16]*Msg)
		c.fid = make(map[uint32]*Fid)
		vprintf("incoming call on %v\n", c.conn.LocalAddr())
		go cfg.conninthread(&c)
		go cfg.connoutthread(&c)
	}
}

func send9pmsg(m *Msg) {
	m.c.outq.send(m)
}

func (cfg *Config) sendomsg(m *Msg) {
	cfg.outq.send(m)
}

func send9pError(m *Msg, ename string) {
	m.rx.Type = plan9.Rerror
	m.rx.Ename = ename
	m.rx.Tag = m.tx.Tag
	send9pmsg(m)
}

func (cfg *Config) conninthread(c *conn) {
	var ok bool

	for {
		m, err := cfg.mread9p(c.conn)
		if err != nil {
			break
		}
		vvprintf("fd#%d -> %v\n", c.conn, &m.tx)
		m.c = c
		m.ctag = m.tx.Tag
		c.nmsg++
		vvprintf("fd#%d: new msg %p\n", c.conn, m)
		if _, ok := c.tag[m.tx.Tag]; ok {
			send9pError(m, "duplicate tag")
			continue
		}
		c.tag[m.tx.Tag] = m

		msgincref(m)
		switch m.tx.Type {
		case plan9.Tversion:
			m.rx.Tag = m.tx.Tag
			m.rx.Msize = m.tx.Msize
			if m.rx.Msize > cfg.msize {
				m.rx.Msize = cfg.msize
			}
			m.rx.Version = "9P2000"
			m.rx.Type = plan9.Rversion
			send9pmsg(m)
			continue

		case plan9.Tflush:
			m.oldm, ok = c.tag[m.tx.Oldtag]
			if !ok {
				m.rx.Tag = m.tx.Tag
				m.rx.Type = plan9.Rflush
				send9pmsg(m)
				continue
			}
			msgincref(m.oldm)

		case plan9.Tattach:
			m.afid = nil
			if m.tx.Afid != plan9.NOFID {
				m.afid, ok = c.fid[m.tx.Afid]
				if !ok {
					send9pError(m, "unknown fid")
					continue
				}
			}
			if m.afid != nil {
				m.afid.ref++
			}
			m.fid = cfg.fidnew(m.tx.Fid)
			if _, ok := c.fid[m.tx.Fid]; ok {
				send9pError(m, "duplicate fid")
				continue
			}
			c.fid[m.tx.Fid] = m.fid

			m.fid.ref++

		case plan9.Twalk:
			m.fid, ok = c.fid[m.tx.Fid]
			if !ok {
				send9pError(m, "unknown fid")
				continue
			}
			m.fid.ref++
			if m.tx.Newfid == m.tx.Fid {
				m.fid.ref++
				m.newfid = m.fid
			} else {
				m.newfid = cfg.fidnew(m.tx.Newfid)
				if _, ok := c.fid[m.tx.Newfid]; ok {
					send9pError(m, "duplicate fid")
					continue
				}
				c.fid[m.tx.Newfid] = m.newfid
				m.newfid.ref++
			}

		case plan9.Tauth:
			if cfg.NoAuth {
				send9pError(m, "authentication rejected")
				continue
			}
			m.afid = cfg.fidnew(m.tx.Afid)
			if _, ok := c.fid[m.tx.Afid]; ok {
				send9pError(m, "duplicate fid")
				continue
			}
			c.fid[m.tx.Afid] = m.afid
			m.afid.ref++

		case plan9.Tcreate:
			if m.tx.Perm&(plan9.DMSYMLINK|plan9.DMDEVICE|plan9.DMNAMEDPIPE|plan9.DMSOCKET) != 0 {
				send9pError(m, "unsupported file type")
				continue
			}
			fallthrough
		case plan9.Topen, plan9.Tclunk, plan9.Tread, plan9.Twrite, plan9.Tremove, plan9.Tstat, plan9.Twstat:
			m.fid, ok = c.fid[m.tx.Fid]
			if !ok {
				send9pError(m, "unknown fid")
				continue
			}
			m.fid.ref++
		}

		// have everything - translate and send
		m.c = c
		m.ctag = m.tx.Tag
		m.tx.Tag = m.tag
		if m.fid != nil {
			m.tx.Fid = m.fid.fid
		}
		if m.newfid != nil {
			m.tx.Newfid = m.newfid.fid
		}
		if m.afid != nil {
			m.tx.Afid = m.afid.fid
		}
		if m.oldm != nil {
			m.tx.Oldtag = m.oldm.tag
		}
		// reference passes to outq
		cfg.outq.send(m)
		for c.nmsg >= maxMsgPerConn {
			c.inputstalled = true
			<-c.inc
		}
	}
	vprintf("fd#%d eof; flushing conn\n", c.conn)

	// flush all outstanding messages
	for _, om := range c.tag {
		msgincref(om) // for us
		m := cfg.msgnew()
		m.internal = true
		m.c = c
		c.nmsg++
		m.tx.Type = plan9.Tflush
		m.tx.Tag = m.tag
		m.tx.Oldtag = om.tag
		m.oldm = om
		msgincref(om)
		msgincref(m) // for outq
		cfg.sendomsg(m)
		mm := <-c.internal
		assert(mm == m)
		cfg.msgput(m) // got from chan
		cfg.msgput(m) // got from msgnew
		if deleteTag(c.tag, om.ctag, om) {
			cfg.msgput(om) // got from hash table
		}
		cfg.msgput(om) // got from msgincref
	}

	//
	// outputthread has written all its messages
	// to the remote connection (because we've gotten all the replies!),
	// but it might not have gotten a chance to msgput
	// the very last one.  sync up to make sure.
	//
	cfg.outq.send(&Msg{
		sync: true,
		c:    c,
	})
	<-c.outqdead

	// everything is quiet; can close the local output queue.
	c.outq.send(nil)
	<-c.outqdead

	// should be no messages left anywhere.
	assert(c.nmsg == 0)

	// clunk all outstanding fids
	for _, f := range c.fid {
		m := cfg.msgnew()
		m.internal = true
		m.c = c
		c.nmsg++
		m.tx.Type = plan9.Tclunk
		m.tx.Tag = m.tag
		m.tx.Fid = f.fid
		m.fid = f
		f.ref++
		msgincref(m)
		cfg.sendomsg(m)
		mm := <-c.internal
		assert(mm == m)
		cfg.msgclear(m)
		cfg.msgput(m) // got from chan
		cfg.msgput(m) // got from msgnew
		cfg.fidput(f) // got from hash table
	}

	assert(c.nmsg == 0)
	c.conn.Close()
	close(c.internal)
	close(c.inc)
}

func (cfg *Config) connoutthread(c *conn) {
	for {
		m := c.outq.recv()
		if m == nil {
			break
		}
		badType := m.tx.Type+1 != m.rx.Type
		switch m.tx.Type {
		case plan9.Tflush:
			om := m.oldm
			if om != nil {
				if deleteTag(om.c.tag, om.ctag, om) {
					cfg.msgput(om)
				}
			}

		case plan9.Tclunk, plan9.Tremove:
			if m.fid != nil {
				if deleteFid(m.c.fid, m.fid.cfid, m.fid) {
					cfg.fidput(m.fid)
				}
			}

		case plan9.Tauth:
			if badType && m.afid != nil {
				vprintf("auth error\n")
				if deleteFid(m.c.fid, m.afid.cfid, m.afid) {
					cfg.fidput(m.afid)
				}
			}

		case plan9.Tattach:
			if badType && m.fid != nil {
				if deleteFid(m.c.fid, m.fid.cfid, m.fid) {
					cfg.fidput(m.fid)
				}
			}

		case plan9.Twalk:
			if badType || len(m.rx.Wqid) < len(m.tx.Wname) {
				if m.tx.Fid != m.tx.Newfid && m.newfid != nil {
					if deleteFid(m.c.fid, m.newfid.cfid, m.newfid) {
						cfg.fidput(m.newfid)
					}
				}
			}

		case plan9.Tread:
		case plan9.Tstat:
		case plan9.Topen:
		case plan9.Tcreate:
		}
		if deleteTag(m.c.tag, m.ctag, m) {
			cfg.msgput(m)
		}
		vvprintf("fd#%d <- %v\n", c.conn, &m.rx)
		rpkt, err := m.rx.Bytes()
		if err != nil {
			log.Fatalf("failed to convert Fcall to bytes: %v\n", err)
		}
		if _, err := c.conn.Write(rpkt); err != nil {
			vprintf("write error: %v\n", err)
		}
		cfg.msgput(m)
		if c.inputstalled && c.nmsg < maxMsgPerConn {
			c.inc <- struct{}{}
		}
	}
	c.outq = nil
	c.outqdead <- struct{}{}
}

func (cfg *Config) outputthread() {
	for {
		m := cfg.outq.recv()
		if m == nil {
			break
		}
		if m.sync {
			m.c.outqdead <- struct{}{}
			continue
		}
		vvprintf("* <- %v\n", &m.tx)
		tpkt, err := m.tx.Bytes()
		if err != nil {
			log.Fatalf("failed to convert Fcall to bytes: %v\n", err)
		}
		if _, err := cfg.Writer.Write(tpkt); err != nil {
			log.Fatalf("output error: %s\n", err)
		}
		cfg.msgput(m)
	}
	log.Printf("output eof\n")
	os.Exit(0)
}

func (cfg *Config) inputthread() {
	vprintf("input thread\n")

	for {
		f, err := plan9.ReadFcall(cfg.Reader)
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatalf("ReadFcall failed: %v", err)
		}
		m := cfg.msgget(int(f.Tag))
		if m == nil {
			log.Printf("unexpected 9P response tag %v\n", f.Tag)
			continue
		}
		m.rx = *f
		vvprintf("* -> %v internal=%v\n", &m.rx, m.internal)
		m.rx.Tag = m.ctag
		if m.internal {
			m.c.internal <- m
		} else if m.c.outq != nil {
			m.c.outq.send(m)
		} else {
			cfg.msgput(m)
		}
	}
	os.Exit(0)
}

func (cfg *Config) fidnew(cfid uint32) *Fid {
	if cfg.freefid == nil {
		cfg.freefid = &Fid{
			fid: uint32(len(cfg.fidtab)),
		}
		cfg.fidtab = append(cfg.fidtab, cfg.freefid)
	}
	f := cfg.freefid
	cfg.freefid = f.next
	f.cfid = cfid
	f.ref = 1
	return f
}

func (cfg *Config) fidput(f *Fid) {
	if f == nil {
		return
	}
	assert(f.ref > 0)
	f.ref--
	if f.ref > 0 {
		return
	}
	f.next = cfg.freefid
	f.cfid = ^uint32(0)
	cfg.freefid = f
}

func msgincref(m *Msg) {
	vvprintf("msgincref %p tag %d/%d ref %d=>%d\n",
		m, m.tag, m.ctag, m.ref, m.ref+1)
	m.ref++
}

func (cfg *Config) msgnew() *Msg {
	if cfg.freemsg == nil {
		cfg.freemsg = &Msg{
			tag: uint16(len(cfg.msgtab)),
		}
		cfg.msgtab = append(cfg.msgtab, cfg.freemsg)
	}
	m := cfg.freemsg
	cfg.freemsg = m.next
	m.ref = 1
	vvprintf("msgnew %p tag %d ref %d\n", m, m.tag, m.ref)
	cfg.nmsg++
	return m
}

// Msgclear clears data associated with connections, so that
// if all msgs have been msgcleared, the connection can be freed.
// The io write thread might still be holding a ref to msg
// even once the connection has finished with it.
func (cfg *Config) msgclear(m *Msg) {
	if m.c != nil {
		m.c.nmsg--
		m.c = nil
	}
	if m.oldm != nil {
		cfg.msgput(m.oldm)
		m.oldm = nil
	}
	if m.fid != nil {
		cfg.fidput(m.fid)
		m.fid = nil
	}
	if m.afid != nil {
		cfg.fidput(m.afid)
		m.afid = nil
	}
	if m.newfid != nil {
		cfg.fidput(m.newfid)
		m.newfid = nil
	}
}

func (cfg *Config) msgput(m *Msg) {
	if m == nil {
		return
	}
	vvprintf("msgput %p tag %d/%d ref %d\n",
		m, m.tag, m.ctag, m.ref)
	assert(m.ref > 0)
	m.ref--
	if m.ref > 0 {
		return
	}
	cfg.nmsg--
	cfg.msgclear(m)
	m.internal = false
	m.next = cfg.freemsg
	cfg.freemsg = m
}

func (cfg *Config) msgget(n int) *Msg {
	if n < 0 || n >= len(cfg.msgtab) {
		return nil
	}
	m := cfg.msgtab[n]
	if m.ref == 0 {
		return nil
	}
	vprintf("msgget %d = %p\n", n, m)
	msgincref(m)
	return m
}

type Qel struct {
	next *Qel
	p    *Msg
}

type Queue struct {
	lk   sync.Mutex
	r    *sync.Cond
	head *Qel
	tail *Qel
}

func newQueue() *Queue {
	var q Queue
	q.r = sync.NewCond(&q.lk)
	return &q
}

func (q *Queue) send(p *Msg) int {
	var e Qel

	q.lk.Lock()
	e.p = p
	e.next = nil
	if q.head == nil {
		q.head = &e
	} else {
		q.tail.next = &e
	}
	q.tail = &e
	q.r.Signal()
	q.lk.Unlock()
	return 0
}

func (q *Queue) recv() *Msg {
	q.lk.Lock()
	for q.head == nil {
		q.r.Wait()
	}
	e := q.head
	q.head = e.next
	q.lk.Unlock()
	p := e.p
	return p
}

func (cfg *Config) mread9p(r io.Reader) (*Msg, error) {
	f, err := plan9.ReadFcall(r)
	if err != nil {
		return nil, err
	}
	m := cfg.msgnew()
	m.tx = *f
	return m, nil
}

/*
int
ignorepipe(void *v, char *s)
{
	USED(v);
	if(strcmp(s, "sys: write on closed pipe") == 0)
		return 1;
	if(strcmp(s, "sys: tstp") == 0)
		return 1;
	if(strcmp(s, "sys: window size change") == 0)
		return 1;
	fprint(2, "9pserve %s: note: %s\n", addr, s);
	return 0;
}
*/

func vprintf(format string, a ...interface{}) {
	if verbose > 0 {
		log.Printf(format, a...)
	}
}

func vvprintf(format string, a ...interface{}) {
	if verbose > 1 {
		log.Printf(format, a...)
	}
}

func assert(b bool) {
	if !b {
		panic("assert failed")
	}
}

func deleteTag(tab map[uint16]*Msg, tag uint16, m *Msg) bool {
	if m1, ok := tab[tag]; ok {
		if m1 != m {
			vprintf("deleteTag %d got %p want %p\n", tag, m1, m)
		}
		delete(tab, tag)
		return true
	}
	return false
}

func deleteFid(tab map[uint32]*Fid, fid uint32, f *Fid) bool {
	if f1, ok := tab[fid]; ok {
		if f1 != f {
			vprintf("deleteFid %d got %p want %p\n", fid, f1, f)
		}
		delete(tab, fid)
		return true
	}
	return false
}
