package netutil

import (
	"fmt"
	"net"

	"encoding/binary"

	"sync"

	"time"

	"sync/atomic"

	"github.com/xiaonanln/goworld/consts"
	"github.com/xiaonanln/goworld/gwlog"
	"github.com/xiaonanln/goworld/opmon"
)

const ( // Three different level of packet size
	PACKET_SIZE_SMALL = 1024
	PACKET_SIZE_BIG   = 1024 * 64
	PACKET_SIZE_HUGE  = 1024 * 1024 * 4
)

const (
	MAX_PACKET_SIZE    = 4 * 1024 * 1024
	SIZE_FIELD_SIZE    = 4
	PREPAYLOAD_SIZE    = SIZE_FIELD_SIZE
	MAX_PAYLOAD_LENGTH = MAX_PACKET_SIZE - PREPAYLOAD_SIZE
)

var (
	NETWORK_ENDIAN = binary.LittleEndian
	errRecvAgain   = _ErrRecvAgain{}
)

type _ErrRecvAgain struct{}

func (err _ErrRecvAgain) Error() string {
	return "recv again"
}

func (err _ErrRecvAgain) Temporary() bool {
	return true
}

func (err _ErrRecvAgain) Timeout() bool {
	return false
}

type PacketConnection struct {
	conn               Connection
	pendingPackets     []*Packet
	pendingPacketsLock sync.Mutex

	payloadLenBuf         [SIZE_FIELD_SIZE]byte
	payloadLenBytesRecved int
	recvTotalPayloadLen   uint32
	recvedPayloadLen      uint32
	recvingPacket         *Packet
}

func NewPacketConnection(conn Connection) *PacketConnection {
	pc := &PacketConnection{
		conn: conn,
	}
	return pc
}

func NewPacketWithPayloadLen(payloadLen uint32) *Packet {
	return allocPacket(payloadLen)
}

func NewPacket() *Packet {
	return allocPacket(INITIAL_PACKET_CAPACITY)
}

func (pc *PacketConnection) NewPacket() *Packet {
	return allocPacket(INITIAL_PACKET_CAPACITY)
}

func (pc *PacketConnection) SendPacket(packet *Packet) error {
	if consts.DEBUG_PACKETS {
		gwlog.Debug("%s SEND PACKET: msgtype=%v, payload=%v", pc, PACKET_ENDIAN.Uint16(packet.bytes[PREPAYLOAD_SIZE:PREPAYLOAD_SIZE+2]),
			packet.bytes[PREPAYLOAD_SIZE+2:PREPAYLOAD_SIZE+packet.GetPayloadLen()])
	}
	if atomic.LoadInt64(&packet.refcount) <= 0 {
		gwlog.Panicf("sending packet with refcount=%d", packet.refcount)
	}

	packet.AddRefCount(1)
	pc.pendingPacketsLock.Lock()
	pc.pendingPackets = append(pc.pendingPackets, packet)
	pc.pendingPacketsLock.Unlock()
	return nil
}

func (pc *PacketConnection) Flush() error {
	pc.pendingPacketsLock.Lock()
	if len(pc.pendingPackets) == 0 { // no packets to send, common to happen, so handle efficiently
		pc.pendingPacketsLock.Unlock()
		return nil
	}
	packets := make([]*Packet, 0, len(pc.pendingPackets))
	packets, pc.pendingPackets = pc.pendingPackets, packets
	pc.pendingPacketsLock.Unlock()

	// flush should only be called in one goroutine
	op := opmon.StartOperation("FlushPackets")

	for _, packet := range packets { // TODO: merge packets and write in one syscall?
		err := WriteAll(pc.conn, packet.data())
		packet.Release()
		if err != nil {
			return err
		}
	}

	op.Finish(time.Millisecond * 100)
	return nil
}

func (pc *PacketConnection) SetRecvDeadline(deadline time.Time) error {
	return pc.conn.SetReadDeadline(deadline)
}

func (pc *PacketConnection) RecvPacket() (*Packet, error) {
	if pc.payloadLenBytesRecved < SIZE_FIELD_SIZE {
		// receive more of payload len bytes
		n, err := pc.conn.Read(pc.payloadLenBuf[pc.payloadLenBytesRecved:])
		pc.payloadLenBytesRecved += n
		if pc.payloadLenBytesRecved < SIZE_FIELD_SIZE {
			if err == nil {
				err = errRecvAgain
			}
			return nil, err // packet not finished yet
		}

		pc.recvTotalPayloadLen = NETWORK_ENDIAN.Uint32(pc.payloadLenBuf[:])

		if pc.recvTotalPayloadLen == 0 || pc.recvTotalPayloadLen > MAX_PAYLOAD_LENGTH {
			// todo: reset the connection when packet size is invalid
			pc.resetRecvStates()
			return nil, fmt.Errorf("invalid payload length: %v", pc.recvTotalPayloadLen)
		}

		pc.recvedPayloadLen = 0
		pc.recvingPacket = NewPacketWithPayloadLen(pc.recvTotalPayloadLen)
	}

	// now all bytes of payload len is received, start receiving payload
	n, err := pc.conn.Read(pc.recvingPacket.bytes[PREPAYLOAD_SIZE+pc.recvedPayloadLen : PREPAYLOAD_SIZE+pc.recvTotalPayloadLen])
	pc.recvedPayloadLen += uint32(n)

	if pc.recvedPayloadLen == pc.recvTotalPayloadLen {
		// full packet received, return the packet
		packet := pc.recvingPacket
		packet.SetPayloadLen(pc.recvTotalPayloadLen)
		pc.resetRecvStates()
		return packet, nil
	}

	if err == nil {
		err = errRecvAgain
	}
	return nil, err
}
func (pc *PacketConnection) resetRecvStates() {
	pc.payloadLenBytesRecved = 0
	pc.recvTotalPayloadLen = 0
	pc.recvingPacket = nil
}

func (pc *PacketConnection) Close() {
	pc.conn.Close()
}

func (pc *PacketConnection) RemoteAddr() net.Addr {
	return pc.conn.RemoteAddr()
}

func (pc *PacketConnection) LocalAddr() net.Addr {
	return pc.conn.LocalAddr()
}

func (pc *PacketConnection) String() string {
	return fmt.Sprintf("[%s >>> %s]", pc.LocalAddr(), pc.RemoteAddr())
}
