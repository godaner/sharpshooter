package core

import (
	"errors"
	"fmt"
	"net"
	"sharpshooter/protocol"
	"sync"
	"sync/atomic"
	"time"
)

const (
	shooting = 1 << iota
	waittimeout
)

var (
	CLOSEERROR = errors.New("the connection is closed")
)

type Sniper struct {
	aim           *net.UDPAddr
	conn          *net.UDPConn
	ammoBag       []*protocol.Ammo
	ammoBagCach   chan protocol.Ammo
	beShotAmmoBag []*protocol.Ammo
	//maxAmmoBag           int
	maxWindows           int
	sendid               uint32
	beShotCurrentId      uint32
	currentWindowStartId uint32
	currentWindowEndId   uint32
	timeout              int64
	shootTime            int64
	shootStatus          int32
	handshakesign        chan struct{}
	readblock            chan struct{}
	timeoutticker        *time.Ticker
	mu                   sync.RWMutex
	bemu                 sync.Mutex
	ackpool              map[uint32]struct{}
	ackmu                sync.Mutex
	acksign              chan struct{}
	closeChan            chan struct{}
	loadsign             chan struct{}
	isClose              bool
}

func NewSniper(conn *net.UDPConn, aim *net.UDPAddr, timeout int64) *Sniper {
	sn := &Sniper{
		aim:           aim,
		conn:          conn,
		timeout:       timeout,
		beShotAmmoBag: make([]*protocol.Ammo, 100),
		handshakesign: make(chan struct{}, 1),
		readblock:     make(chan struct{}, 0),
		//maxAmmoBag:    100,
		maxWindows:  100,
		mu:          sync.RWMutex{},
		bemu:        sync.Mutex{},
		ackpool:     map[uint32]struct{}{},
		acksign:     make(chan struct{}, 0),
		ammoBagCach: make(chan protocol.Ammo, 100),
		closeChan:   make(chan struct{}, 0),
		loadsign:    make(chan struct{}, 1),
	}
	go sn.load()
	return sn

}

func (s *Sniper) load() {

	for {

		ammo, ok := <-s.ammoBagCach
		if !ok {
			return
		}
		s.mu.Lock()
	loop:
		if len(s.ammoBag) >= s.maxWindows {
			s.mu.Unlock()
			if atomic.LoadInt32(&s.shootStatus)&(shooting|waittimeout) == 0 {
				go s.Shot()
			}
			<-s.loadsign
			s.mu.Lock()
			goto loop
		}

		s.ammoBag = append(s.ammoBag, &ammo)

		if atomic.LoadInt32(&s.shootStatus)&(shooting|waittimeout) == 0 {
			fmt.Println("in")
			go s.Shot()
		}
		s.mu.Unlock()

	}
}

func (s *Sniper) Shot() {

loop:
	status := atomic.LoadInt32(&s.shootStatus)
	if status&shooting >= 1 {
		fmt.Println("out")
		return
	}

	if !atomic.CompareAndSwapInt32(&s.shootStatus, status, status|shooting) {
		fmt.Println("out1")
		return
	}

	if s.timeout == 0 {
		s.timeout = int64(time.Hour)
	}

	s.timeoutticker = time.NewTicker(time.Duration(s.timeout) * time.Nanosecond)

	s.mu.Lock()
	if s.ammoBag == nil || len(s.ammoBag) == 0 {
		atomic.StoreInt32(&s.shootStatus, 0)
		s.timeoutticker.Stop()
		s.mu.Unlock()
		fmt.Println("out2")
		return
	}

	for k, _ := range s.ammoBag {

		if s.ammoBag[k] == nil {
			continue
		}

		if k > s.maxWindows {
			break
		}

		s.currentWindowEndId = s.currentWindowStartId + uint32(k)

		_, err := s.conn.WriteToUDP(protocol.Marshal(*s.ammoBag[k]), s.aim)

		if err != nil {
			panic(err)
		}
	}

	for {
		status = atomic.LoadInt32(&s.shootStatus)
		if atomic.CompareAndSwapInt32(&s.shootStatus, status, (status&(^shooting))|waittimeout) {
			break
		}
	}

	s.mu.Unlock()

	if s.isClose {
		close(s.closeChan)
	}

	select {

	case <-s.timeoutticker.C:

		for {
			status = atomic.LoadInt32(&s.shootStatus)
			if atomic.CompareAndSwapInt32(&s.shootStatus, status, (status & (^waittimeout))) {
				break
			}
		}

		goto loop

	case <-s.closeChan:
		return

	}

}

func (s *Sniper) ack(id uint32) {

	s.ackmu.Lock()
	defer s.ackmu.Unlock()
	s.ackpool[id] = struct{}{}

	select {
	case s.acksign <- struct{}{}:
	default:
	}

}

func (s *Sniper) ackSender() {

	timer := time.NewTimer(time.Millisecond * 100)
	for {

		if len(s.ackpool) == 0 {
			<-s.acksign
		}
		s.ackmu.Lock()
		for k, _ := range s.ackpool {

			ammo := protocol.Ammo{
				Id:   k,
				Kind: protocol.ACK,
				Body: nil,
			}
			_, err := s.conn.WriteToUDP(protocol.Marshal(ammo), s.aim)
			if err != nil {
				panic(err)
			}
			delete(s.ackpool, k)
		}
		s.ackmu.Unlock()

		<-timer.C
		timer.Reset(time.Duration(s.timeout) / 10 * time.Nanosecond)

	}
}

func (s *Sniper) score(id uint32) {

	fmt.Println("id", id)
	fmt.Println("starid", s.currentWindowStartId)
	fmt.Println("endid", s.currentWindowEndId)
	fmt.Println("len", len(s.ammoBag))

	if id < atomic.LoadUint32(&s.currentWindowStartId) {
		return
	}

	index := int(id) - int(atomic.LoadUint32(&s.currentWindowStartId))

	if index > len(s.ammoBag) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if index > s.maxWindows {
		s.ammoBag = s.ammoBag[index:]
		atomic.AddUint32(&s.currentWindowStartId, uint32(index))
		select {
		case s.loadsign <- struct{}{}:
		default:
		}
		return
	}

	if len(s.ammoBag) > index && s.ammoBag[index].AckAdd() > 3 && s.currentWindowStartId != s.currentWindowEndId {
		fmt.Println("fast reshot")
		_, err := s.conn.WriteToUDP(protocol.Marshal(*s.ammoBag[index]), s.aim)
		if err != nil {
			panic(err)
		}
	}

	s.ammoBag = s.ammoBag[index:]

	select {
	case s.loadsign <- struct{}{}:
	default:
	}

	atomic.AddUint32(&s.currentWindowStartId, uint32(index))

	if s.currentWindowStartId >= s.currentWindowEndId {
		if atomic.LoadInt32(&s.shootStatus)&(shooting) == 0 {
			fmt.Println("in")
			go s.Shot()
		} else {
			fmt.Println("bad int ", atomic.LoadInt32(&s.shootStatus))
		}

	}

}

func (s *Sniper) BeShot(ammo *protocol.Ammo) {

	s.bemu.Lock()
	defer s.bemu.Unlock()

	bagIndex := ammo.Id - s.beShotCurrentId

	if ammo.Id < s.beShotCurrentId {

		fmt.Println("ammo.Id < s.beShotCurrentId", ammo.Id)
		var id uint32
		for k, v := range s.beShotAmmoBag {

			if v == nil {
				id = s.beShotCurrentId + uint32(k)
				break
			}

		}

		if id == 0 {
			id = s.beShotCurrentId + uint32(len(s.beShotAmmoBag))
		}

		fmt.Println("nid ", id)
		s.ack(id)

		return
	}

	if int(bagIndex) >= len(s.beShotAmmoBag) {
		fmt.Println("int(bagIndex) >= len(s.beShotAmmoBag)", ammo.Id)
		return
	}

	if s.beShotAmmoBag[bagIndex] == nil {
		s.beShotAmmoBag[bagIndex] = ammo
	}

	var nid uint32
	for k, v := range s.beShotAmmoBag {

		if v == nil {
			nid = s.beShotCurrentId + uint32(k)
			break
		}

	}

	if nid == 0 {
		nid = s.beShotCurrentId + uint32(len(s.beShotAmmoBag))
	}

	select {

	case s.readblock <- struct{}{}:

	default:

	}

	fmt.Println("nid ", nid)
	s.ack(nid)
}

func PrintAmmo(s []*protocol.Ammo) {

	for _, ammo := range s {

		if ammo == nil || ammo.Body == nil {
			continue
		}
		fmt.Print(string(ammo.Body))
		fmt.Print("  ")

	}
}

func (s *Sniper) Read(b []byte) (n int, err error) {

loop:

	s.bemu.Lock()
	for len(s.beShotAmmoBag) != 0 && s.beShotAmmoBag[0] != nil {

		if s.beShotAmmoBag[0] == nil {
			break
		}

		n += copy(b[n:], s.beShotAmmoBag[0].Body)

		if n == len(b) {
			if n < len(s.beShotAmmoBag[0].Body) {
				s.beShotAmmoBag[0].Body = s.beShotAmmoBag[0].Body[n:]
			} else {
				//PrintAmmo(s.beShotAmmoBag)
				s.beShotAmmoBag = append(s.beShotAmmoBag[1:], nil)
				//fmt.Println("ed")
				//PrintAmmo(s.beShotAmmoBag)
				s.beShotCurrentId++
			}
			break

		} else {

			//PrintAmmo(s.beShotAmmoBag)

			s.beShotAmmoBag = append(s.beShotAmmoBag[1:], nil)

			//fmt.Println("ed")
			//PrintAmmo(s.beShotAmmoBag)

			s.beShotCurrentId++
			continue
		}
	}

	s.bemu.Unlock()

	if n <= 0 {
		select {
		case <-s.readblock:
			goto loop

		case <-s.closeChan:
			return 0, CLOSEERROR

		}

	}

	//fmt.Println(b)
	//fmt.Println(string(b))
	return
}

func (s *Sniper) Write(b []byte) (n int, err error) {

	id := atomic.AddUint32(&s.sendid, 1)

	s.ammoBagCach <- protocol.Ammo{
		Length: 0,
		Id:     id - 1,
		Kind:   protocol.NORMAL,
		Body:   b,
	}

	return len(b), nil
}

func (s *Sniper) Close() error {

	s.ammoBagCach <- protocol.Ammo{
		Kind: protocol.CLOSE,
		Body: nil,
	}

	<-s.closeChan

	return s.conn.Close()
}
