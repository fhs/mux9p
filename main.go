// This program announces and multiplexes a 9P service.
//
// It is a port of Plan 9 Port's 9pserve program.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"9fans.net/go/plan9"
)

const (
	STACK  = 32768
	MAXMSG = 64 // per connection
)

type Fid struct {
	fid     uint32
	ref     int
	cfid    uint32
	offset  int
	coffset int
}

type Msg struct {
	c        *Conn
	internal bool
	sync     bool
	ref      int
	ctag     uint16
	tag      uint16
	tx       plan9.Fcall
	rx       plan9.Fcall
	fid      *Fid
	newfid   *Fid
	afid     *Fid
	oldm     *Msg
	next     *Msg
	//tpkt     []byte
	//rpkt     []byte
}

type Conn struct {
	conn         net.Conn
	nmsg         int
	inc          chan struct{}
	internal     chan *Msg
	inputstalled bool
	tag          map[uint16]*Msg
	fid          map[uint32]*Fid
	outq         *Queue
	inq          *Queue
	outqdead     chan struct{}
}

var (
	outq      *Queue // stdout Msg queue
	inq       *Queue // stdin Msg queue
	msize     uint32 = 8092
	versioned bool

	noauth  = flag.Bool("n", false, "no authentication; respond to Tauth messages with an error")
	verbose = flag.Int("v", 0, "verbosity")
	logging = flag.Bool("l", false, "logging; write a debugging log to addr.log")
)

func usage() {
	fmt.Fprintf(os.Stderr, "usage: 9pserve [flags] address\n")
	fmt.Fprintf(os.Stderr, "\treads/writes 9P messages on stdin/stdout\n")
	fmt.Fprintf(os.Stderr, "\n")
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	x := os.Getenv("verbose9pserve")
	if x != "" {
		var err error
		*verbose, err = strconv.Atoi(x)
		if err != nil {
			*verbose = 0
		}
		fmt.Fprintf(os.Stderr, "verbose9pserve %s => %d\n", x, verbose)
	}

	flag.Parse()
	if flag.NArg() != 1 {
		usage()
	}
	addr := flag.Arg(0)

	network, netaddr := parseAddr(addr)
	ln, err := net.Listen(network, netaddr)
	if err != nil {
		log.Fatalf("listen failed: %v\n", err)
	}
	defer ln.Close()

	if *logging {
		if strings.HasPrefix(addr, "unix!") {
			addr = addr[len("unix!"):]
		}
		f, err := os.Create(fmt.Sprintf("%s.log", addr))
		if err != nil {
			log.Fatalf("create failed: %v", err)
		}
		defer f.Close()
		log.SetOutput(f)
	}
	vprintf("9pserve running\n")
	mainproc(ln)
}

func mainproc(ln net.Listener) {
	//atnotify(ignorepipe, 1)

	outq = newQueue()
	inq = newQueue()

	f := new(plan9.Fcall)
	if !versioned {
		f.Type = plan9.Tversion
		f.Version = "9P2000"
		f.Msize = msize
		f.Tag = plan9.NOTAG
		vbuf, err := f.Bytes()
		if err != nil {
			log.Fatalf("Fcall conversion to bytes failed: %v", err)
		}
		vvprintf("* <- %v\n", &f)
		_, err = os.Stdout.Write(vbuf)
		if err != nil {
			log.Fatalf("error writing Tversion: %v", err)
		}
		f, err = plan9.ReadFcall(os.Stdin)
		if err != nil {
			log.Fatalf("ReadFcall failed: %v", err)
		}
		if f.Msize < msize {
			msize = f.Msize
		}
		vvprintf("* -> %v\n", &f)
	}

	go inputthread()
	go outputthread()

	listenthread(ln)
}

func listenthread(ln net.Listener) {
	for {
		var c Conn
		var err error
		c.conn, err = ln.Accept()
		if err != nil {
			vprintf("listen: %v\n", err)
			return
		}
		c.inc = make(chan struct{})
		c.internal = make(chan *Msg)
		c.inq = newQueue()
		c.outq = newQueue()
		c.outqdead = make(chan struct{})
		vprintf("incoming call on %v\n", c.conn.LocalAddr())
		go conninthread(&c)
		go connoutthread(&c)
	}
}

func send9pmsg(m *Msg) {
	//rpkt, err := m.rx.Bytes()
	//if err != nil {
	//	log.Fatalf("failed to convert Fcall to bytes: %v\n", err)
	//}
	m.c.outq.send(m)
}

func sendomsg(m *Msg) {
	//tpkt, err := m.tx.Bytes()
	//if err != nil {
	//	log.Fatalf("failed to convert Fcall to bytes: %v\n", err)
	//}
	outq.send(m)
}

func send9pError(m *Msg, ename string) {
	m.rx.Type = plan9.Rerror
	m.rx.Ename = ename
	m.rx.Tag = m.tx.Tag
	send9pmsg(m)
}

func conninthread(c *Conn) {
	var (
		sync Msg
		ok   bool
	)

	for {
		m, err := mread9p(c.conn)
		if err != nil {
			break
		}
		vvprintf("fd#%d -> %F\n", c.conn, &m.tx)
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
			if m.rx.Msize > msize {
				m.rx.Msize = msize
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
			m.fid = fidnew(m.tx.Fid)
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
				m.newfid = fidnew(m.tx.Newfid)
				if _, ok := c.fid[m.tx.Newfid]; ok {
					send9pError(m, "duplicate fid")
					continue
				}
				c.fid[m.tx.Newfid] = m.newfid
				m.newfid.ref++
			}

		case plan9.Tauth:
			if *noauth {
				send9pError(m, "authentication rejected")
				continue
			}
			m.afid = fidnew(m.tx.Afid)
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
		outq.send(m)
		for c.nmsg >= MAXMSG {
			c.inputstalled = true
			<-c.inc
		}
	}
	vprintf("fd#%d eof; flushing conn\n", c.conn)

	// flush all outstanding messages
	for _, om := range c.tag {
		msgincref(om) // for us
		m := msgnew(0)
		m.internal = true
		m.c = c
		c.nmsg++
		m.tx.Type = plan9.Tflush
		m.tx.Tag = m.tag
		m.tx.Oldtag = om.tag
		m.oldm = om
		msgincref(om)
		msgincref(m) // for outq
		sendomsg(m)
		mm := <-c.internal
		assert(mm == m)
		msgput(m) // got from recvp
		msgput(m) // got from msgnew
		if deleteTag(c.tag, om.ctag, om) {
			msgput(om) // got from hash table
		}
		msgput(om) // got from msgincref
	}

	//
	// outputthread has written all its messages
	// to the remote connection (because we've gotten all the replies!),
	// but it might not have gotten a chance to msgput
	// the very last one.  sync up to make sure.
	//
	sync.sync = true
	sync.c = c
	outq.send(&sync)
	<-c.outqdead

	// everything is quiet; can close the local output queue.
	c.outq.send(nil)
	<-c.outqdead

	// should be no messages left anywhere.
	assert(c.nmsg == 0)

	// clunk all outstanding fids
	for _, f := range c.fid {
		m := msgnew(0)
		m.internal = true
		m.c = c
		c.nmsg++
		m.tx.Type = plan9.Tclunk
		m.tx.Tag = m.tag
		m.tx.Fid = f.fid
		m.fid = f
		f.ref++
		msgincref(m)
		sendomsg(m)
		mm := <-c.internal
		assert(mm == m)
		msgclear(m)
		msgput(m) // got from recvp
		msgput(m) // got from msgnew
		fidput(f) // got from hash table
	}

	assert(c.nmsg == 0)
	c.conn.Close()
	close(c.internal)
	close(c.inc)
	c.inq = nil
}

func connoutthread(c *Conn) {
	var (
		err   bool
		m, om *Msg
	)

	for {
		m = c.outq.recv()
		if m == nil {
			break
		}
		err = m.tx.Type+1 != m.rx.Type
		switch m.tx.Type {
		case plan9.Tflush:
			om = m.oldm
			if om != nil {
				if deleteTag(om.c.tag, om.ctag, om) {
					msgput(om)
				}
			}

		case plan9.Tclunk, plan9.Tremove:
			if m.fid != nil {
				if deleteFid(m.c.fid, m.fid.cfid, m.fid) {
					fidput(m.fid)
				}
			}

		case plan9.Tauth:
			if err && m.afid != nil {
				vprintf("auth error\n")
				if deleteFid(m.c.fid, m.afid.cfid, m.afid) {
					fidput(m.afid)
				}
			}

		case plan9.Tattach:
			if err && m.fid != nil {
				if deleteFid(m.c.fid, m.fid.cfid, m.fid) {
					fidput(m.fid)
				}
			}

		case plan9.Twalk:
			if err || len(m.rx.Wqid) < len(m.tx.Wname) {
				if m.tx.Fid != m.tx.Newfid && m.newfid != nil {
					if deleteFid(m.c.fid, m.newfid.cfid, m.newfid) {
						fidput(m.newfid)
					}
				}
			}

		case plan9.Tread:
		case plan9.Tstat:
		case plan9.Topen:
		case plan9.Tcreate:
		}
		if deleteTag(m.c.tag, m.ctag, m) {
			msgput(m)
		}
		vvprintf("fd#%d <- %F\n", c.conn, &m.rx)
		rpkt, err := m.rx.Bytes()
		if err != nil {
			log.Fatalf("failed to convert Fcall to bytes: %v\n", err)
		}
		rewritehdr(&m.rx, rpkt)
		if _, err := c.conn.Write(rpkt); err != nil {
			vprintf("write error: %v\n", err)
		}
		msgput(m)
		if c.inputstalled && c.nmsg < MAXMSG {
			c.inc <- struct{}{}
		}
	}
	c.outq = nil
	c.outqdead <- struct{}{}
}

func outputthread() {
	for {
		m := outq.recv()
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
		rewritehdr(&m.tx, tpkt)
		if _, err := os.Stdout.Write(tpkt); err != nil {
			log.Fatalf("output error: %s\n", err)
		}
		msgput(m)
	}
	log.Printf("output eof\n")
	os.Exit(0)
}

func inputthread() {
	vprintf("input thread\n")

	for {
		f, err := plan9.ReadFcall(os.Stdin)
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatalf("ReadFcall failed: %v", err)
		}
		m := msgget(int(f.Tag))
		if m == nil {
			log.Printf("unexpected 9P response tag %v\n", f.Tag)
			continue
		}
		m.rx = *f
		vvprintf("* -> %v internal=%v\n", &m.rx, m.internal)
		//m.rpkt = pkt
		m.rx.Tag = m.ctag
		if m.internal {
			m.c.internal <- m
		} else if m.c.outq != nil {
			m.c.outq.send(m)
		} else {
			msgput(m)
		}
	}
	os.Exit(0)
}

var freefid = sync.Pool{
	New: func() interface{} {
		return &Fid{
			ref:     1,
			offset:  0,
			coffset: 0,
		}
	},
}

func fidnew(cfid uint32) *Fid {
	f := freefid.Get().(*Fid)
	f.cfid = cfid
	return f
}

func fidput(f *Fid) {
	if f == nil {
		return
	}
	assert(f.ref > 0)
	f.ref--
	if f.ref > 0 {
		return
	}
	f.cfid = ^uint32(0)
	freefid.Put(f)
}

var (
	msgtab  []*Msg
	nmsg    int
	freemsg *Msg
)

func msgincref(m *Msg) {
	vvprintf("msgincref %p tag %d/%d ref %d=>%d\n",
		m, m.tag, m.ctag, m.ref, m.ref+1)
	m.ref++
}

func msgnew(x int) *Msg {
	if freemsg == nil {
		freemsg = &Msg{
			tag: uint16(len(msgtab)),
		}
		msgtab = append(msgtab, freemsg)
	}
	m := freemsg
	freemsg = m.next
	m.ref = 1
	vvprintf("msgnew %p tag %d ref %d\n", m, m.tag, m.ref)
	nmsg++
	return m
}

//
// Clear data associated with connections, so that
// if all msgs have been msgcleared, the connection
// can be freed.  Note that this does *not* free the tpkt
// and rpkt; they are freed in msgput with the msg itself.
// The io write thread might still be holding a ref to msg
// even once the connection has finished with it.
//
func msgclear(m *Msg) {
	if m.c != nil {
		m.c.nmsg--
		m.c = nil
	}
	if m.oldm != nil {
		msgput(m.oldm)
		m.oldm = nil
	}
	if m.fid != nil {
		fidput(m.fid)
		m.fid = nil
	}
	if m.afid != nil {
		fidput(m.afid)
		m.afid = nil
	}
	if m.newfid != nil {
		fidput(m.newfid)
		m.newfid = nil
	}
}

func msgput(m *Msg) {
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
	nmsg--
	msgclear(m)
	m.internal = false
	m.next = freemsg
	freemsg = m
}

func msgget(n int) *Msg {
	if n < 0 || n >= len(msgtab) {
		return nil
	}
	m := msgtab[n]
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

func mread9p(r io.Reader) (*Msg, error) {
	f, err := plan9.ReadFcall(r)
	if err != nil {
		return nil, err
	}
	m := msgnew(0)
	m.tx = *f
	return m, nil
}

func rewritehdr(f *plan9.Fcall, pkt []byte) {
	i := 4 + 1 // length + Type
	i += pbit16(pkt[i:], f.Tag)
	switch f.Type {
	case plan9.Tversion, plan9.Rversion:
		i += 4
		i += pstring(pkt[i:], f.Version)

	case plan9.Tauth:
		i += pbit32(pkt[i:], f.Afid)
		i += pstring(pkt[i:], f.Uname)
		i += pstring(pkt[i:], f.Aname)

	case plan9.Tflush:
		i += pbit16(pkt[i:], f.Oldtag)

	case plan9.Tattach:
		i += pbit32(pkt[i:], f.Fid)
		i += pbit32(pkt[i:], f.Afid)
		i += pstring(pkt[i:], f.Uname)
		i += pstring(pkt[i:], f.Aname)

	case plan9.Twalk:
		i += pbit32(pkt[i:], f.Fid)
		i += pbit32(pkt[i:], f.Newfid)
		i += 2
		for _, wname := range f.Wname {
			i += pstring(pkt[i:], wname)
		}

	case plan9.Tcreate:
		pstring(pkt[i+4:], f.Name)
		fallthrough
	case plan9.Topen, plan9.Tclunk, plan9.Tremove, plan9.Tstat, plan9.Twstat, plan9.Twrite:
		i += pbit32(pkt[i:], f.Fid)

	case plan9.Tread:
		i += pbit32(pkt[i:], f.Fid)
		i += pbit64(pkt[i:], f.Offset)

	case plan9.Rerror:
		i += pstring(pkt[i:], f.Ename)
	}
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
	fprint(2, "9pserve %s: %T note: %s\n", addr, s);
	return 0;
}
*/

func parseAddr(dial string) (net, addr string) {
	if dial == "" {
		panic("empty dial string")
	}
	f := strings.SplitN(dial, "!", 3)
	if f[0] == "net" {
		panic("unsupported network net")
	}
	return f[0], strings.Join(f[1:], ":")
}

func vprintf(format string, a ...interface{}) {
	if *verbose > 0 {
		log.Printf(format, a...)
	}
}

func vvprintf(format string, a ...interface{}) {
	if *verbose > 1 {
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
