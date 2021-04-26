package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

var dead int

// Peer is parsed from 6 bytes, first 4 become IP address, 2 are its exposed Port
type Peer struct {
	IP   net.IP
	Port uint16
}

func (p Peer) String() string {
	return net.JoinHostPort(p.IP.String(), strconv.Itoa(int(p.Port)))
}

// UnmarshalPeers from a tracker response
func UnmarshalPeers(peersBin []byte) ([]Peer, error) {
	const peerSize = 6
	if len(peersBin)%peerSize != 0 {
		return nil, fmt.Errorf("incorrect peersBin length, %d", len(peersBin))
	}
	numPeers := len(peersBin) / peerSize
	peers := make([]Peer, 0, numPeers)
	for i := 0; i < numPeers; i++ {
		offset := i * peerSize
		ipBytes := peersBin[offset : offset+4]
		portBytes := peersBin[offset+4 : offset+6]
		p := Peer{
			IP: net.IP(ipBytes),
			// port is 2 bytes parsed via big endian
			Port: binary.BigEndian.Uint16(portBytes),
		}
		peers = append(peers, p)
	}
	return peers, nil
}

type PeerClient struct {
	Conn     net.Conn // TCP connection to peer
	IsChoked bool
	Bitfield Bitfield // tracks which pieces the peer says it can send us
	// can remove, only needed in handshake?
	InfoHash [20]byte
	// can remove, only needed in handshake?
	MyPeerID     [20]byte
	BadResponses int // after about 5, kill the peer
}

// NewPeerClient initializes a connection with a peer, completing the handshake
func NewPeerClient(peer Peer, infoHash, peerID [20]byte) (*PeerClient, error) {
	conn, err := net.DialTimeout("tcp", peer.String(), 3*time.Second)
	if err != nil {
		return nil, err
	}

	// move handshake and setbitfield into here?
	cli := &PeerClient{
		Conn:     conn,
		InfoHash: infoHash,
		MyPeerID: peerID,
	}

	err = cli.handshake()
	if err != nil {
		return nil, err
	}

	// receive bitfield message
	msg, err := cli.ReadMessage()
	if err != nil {
		return nil, err
	}
	cli.Bitfield = Bitfield(msg.Payload)

	return cli, nil
}

// Attempt to handshake with the underlying Peer
func (p *PeerClient) handshake() error {
	h := Handshake{
		Protocol: "BitTorrent protocol",
		InfoHash: p.InfoHash,
		PeerID:   p.MyPeerID,
	}
	b := h.Serialize()
	// fmt.Printf("sending handshake %q\n", string(b))
	p.Conn.Write(b)

	var resp Handshake
	// fmt.Println("reading handshake")
	err := ReadHandshake(p.Conn, &resp)
	if err != nil {
		fmt.Println("dead @ handshake 1")
		dead++
		return err
	}

	if !bytes.Equal(resp.InfoHash[:], p.InfoHash[:]) {
		fmt.Println("dead @ handshake 2")
		dead++
		return fmt.Errorf("expected infohash %x but got %x", p.InfoHash, resp.InfoHash)
	}

	fmt.Println("successful handshake")

	return nil
}

// ~~ todo move to top somewhere...
// var NoPiece = errors.New("peer does not have piece")

type Job struct {
	Index  int
	Length int
	Hash   [20]byte
}
type PieceResult struct {
	Index     int
	FilePiece []byte
}

func (p *PeerClient) ListenForJobs(jobQueue chan *Job, results chan<- *PieceResult) {
	defer p.Conn.Close()

	// send unchoke
	// send interested
	requestMsg := Message{
		ID: MsgUnchoke,
	}
	_, err := p.Conn.Write(requestMsg.Serialize())
	if err != nil {
		fmt.Println("error sending unchoke", err)
		return
	}

	requestMsg = Message{
		ID: MsgInterested,
	}
	_, err = p.Conn.Write(requestMsg.Serialize())
	if err != nil {
		fmt.Println("error sending interested", err)
		return
	}

	// read off of job queue
	for job := range jobQueue {
		if !p.Bitfield.HasPiece(job.Index) {
			// put back on queue
			jobQueue <- job
			continue // continue to listen for next job
		}

		var requested, received, backlog int
		pieceBuf := make([]byte, job.Length)

		// set a deadline so a stuck client puts its job back on the queue
		p.Conn.SetDeadline(time.Now().Add(time.Second * 30))

		// 16KB in binary
		// const maxBlockSize = 16384
		const maxBlockSize = 16000
		// how many unfulfilled requests to have at one time
		const maxBacklog = 10

	attemptLoop:
		for received < job.Length {
			// create backlog
			for !p.IsChoked && backlog < maxBacklog {
				payload := make([]byte, 12)
				// piece index
				binary.BigEndian.PutUint32(payload[0:4], uint32(job.Index))
				// begin/offset to start block at
				binary.BigEndian.PutUint32(payload[4:8], uint32(requested))
				// length of block to download, last block may not be a full block
				blockSize := maxBlockSize
				if requested+blockSize > job.Length {
					blockSize = job.Length - requested
				}
				binary.BigEndian.PutUint32(payload[8:12], uint32(blockSize))
				requestMsg := Message{
					ID:      MsgRequest,
					Payload: payload,
				}

				// fmt.Println("writing message", requestMsg)

				p.Conn.Write(requestMsg.Serialize())
				requested += maxBlockSize
				backlog++
			}

			msg, err := p.ReadMessage()
			if err != nil {
				fmt.Println("  error reading message", err)
				break attemptLoop
			}
			n, err := p.ProcessMessage(msg, pieceBuf)
			if err != nil {
				fmt.Println("  error processing message", err)
				break attemptLoop
			}
			received += n
			if n != 0 {
				backlog--
			}
		}

		// fmt.Println("piece buffer[0]", pieceBuf[0])

		// check integrity
		pieceHash := sha1.Sum(pieceBuf)
		if !bytes.Equal(pieceHash[:], job.Hash[:]) {
			fmt.Println("    MALFORMED RESPONSE: index, IP.", job.Index, p.Conn.RemoteAddr())
			jobQueue <- job
			dead++
			fmt.Println("   DEAD", dead)
			return
		}

		// tell client we HAVE this piece now

		msg := Message{
			ID:      MsgHave,
			Payload: make([]byte, 4),
		}
		binary.BigEndian.PutUint32(msg.Payload, uint32(job.Index))
		p.Conn.Write(msg.Serialize())

		// write to the results channel for processing
		results <- &PieceResult{
			Index:     job.Index,
			FilePiece: pieceBuf,
		}
	}
}

// Handshake is just a bunch of bytes. It is made of:
//  - Length of protocol identifier: 19 (always '0x13')
//  - Protocol Identifier (pstr), always "BitTorrent protocol"
//  - Reserved Bytes: 8 bytes. (used for indicating support of certain
//    extensions but we support none...)
//  - InfoHash calculated earlier, file identifier
//  - PeerID that identifies ME
type Handshake struct {
	Protocol string
	InfoHash [20]byte
	PeerID   [20]byte
}

// Serialize the handshake to a buffer
func (h Handshake) Serialize() []byte {
	var buf bytes.Buffer
	buf.WriteByte(byte(len(h.Protocol)))
	buf.WriteString(h.Protocol)
	buf.Write(make([]byte, 8))
	buf.Write(h.InfoHash[:])
	buf.Write(h.PeerID[:])

	return buf.Bytes()
}

// Read a stream and return a Handshake
func ReadHandshake(r io.Reader, h *Handshake) error {
	// todo set deadlines for these

	// read 1 byte for the length of protocol message, the rest of the message
	// is 48 bytes
	lengthBuf := make([]byte, 1)
	_, err := io.ReadFull(r, lengthBuf)
	if err != nil {
		return err
	}
	lenProtocolStr := int(lengthBuf[0])
	if lenProtocolStr == 0 {
		return errors.New("zero length protocol string")
	}
	if lenProtocolStr != 19 {
		return fmt.Errorf("handshake read Protocol is not 19, got %d", lenProtocolStr)
	}

	handshakeBuf := make([]byte, lenProtocolStr+48)
	_, err = io.ReadFull(r, handshakeBuf)
	if err != nil {
		return fmt.Errorf("reading to handshake buffer %w", err)
	}

	// parse handshake details into handshake
	h.Protocol = string(handshakeBuf[:lenProtocolStr])
	curr := lenProtocolStr
	curr += 8 // ignore 8 bytes
	copy(h.InfoHash[:], handshakeBuf[curr:curr+20])
	copy(h.PeerID[:], handshakeBuf[curr+20:])

	return nil
}

// MessageID are the types of messages that can be sent
type MessageID uint8

const (
	MsgChoke MessageID = iota
	MsgUnchoke
	MsgInterested
	MsgNotInterested
	MsgHave
	MsgBitfield
	MsgRequest
	MsgPiece
	MsgCancel
)

var messageIDStrings = map[MessageID]string{
	MsgChoke:         "choke",
	MsgUnchoke:       "unchoke",
	MsgInterested:    "interested",
	MsgNotInterested: "not interested",
	MsgHave:          "have",
	MsgBitfield:      "bitfield",
	MsgRequest:       "request",
	MsgPiece:         "piece",
	MsgCancel:        "cancel",
}

func (m MessageID) String() string {
	return messageIDStrings[m]
}

// Message from a Peer has the structure:
// Length 32bit int, 4 bytes -> BigEndian: how many bytes long the message will be
// ID 1 byte: message type, see MessageID type
// Payload (optional), rest of body
type Message struct {
	ID      MessageID
	Payload []byte
}

// Serialize a message into a buffer
func (m *Message) Serialize() []byte {
	if m == nil {
		// keep-alive message
		return make([]byte, 4)
	}
	length := uint32(len(m.Payload) + 1) // +1 for ID
	buf := make([]byte, length+4)        // + 4 to fit <length> at start of message
	binary.BigEndian.PutUint32(buf[0:4], length)
	buf[4] = byte(m.ID)
	copy(buf[5:], m.Payload)
	return buf
}

func (p *PeerClient) ReadMessage() (*Message, error) {
	// 4 bytes that represent the length of the rest of the message
	lengthBuf := make([]byte, 4)
	_, err := io.ReadFull(p.Conn, lengthBuf)
	if err != nil {
		return nil, fmt.Errorf("reading message length: %w", err)
	}
	// SUBTRACT 4 for <length> piece of message
	length := binary.BigEndian.Uint32(lengthBuf)

	if length == 0 {
		// keep-alive message
		return nil, nil
	}

	// buffer to contain the rest of the message, 1 byte for the MessageID type
	// the rest is the payload
	messageBuf := make([]byte, length)
	_, err = io.ReadFull(p.Conn, messageBuf)
	if err != nil {
		return nil, fmt.Errorf("reading message payload: %w", err)
	}

	return &Message{
		ID:      MessageID(messageBuf[0]),
		Payload: messageBuf[1:],
	}, nil
}

// ProcessMessage updates the state of the peer client
func (p *PeerClient) ProcessMessage(msg *Message, pieceBuf []byte) (int, error) {
	if msg == nil {
		return 0, nil
	}
	switch msg.ID {
	case MsgChoke:
		p.IsChoked = true
	case MsgUnchoke:
		p.IsChoked = false
	case MsgInterested:
		// not handling this, we're purely leeches
		fmt.Println("received INTERESTED")
	case MsgNotInterested:
		// not handling this, we're purely leeches
		fmt.Println("received NOT INTERESTED")
	case MsgHave:
		index := binary.BigEndian.Uint32(msg.Payload)
		p.Bitfield.SetPiece(int(index))
	case MsgBitfield:
		p.Bitfield = msg.Payload
	case MsgRequest:
		// not handling this, we're purely leeches
		fmt.Println("received REQUEST")
	case MsgPiece:
		// payload format is 4-byte BigEndian for index, 4-byte BigEndian for begin offset
		// then data of the buffer
		index := binary.BigEndian.Uint32(msg.Payload[0:4])
		_ = index // unused b/c accessible via Job struct
		// unused b/c the pieceBuf is already the index?..
		// should error handle against the job's index?
		begin := binary.BigEndian.Uint32(msg.Payload[4:8])
		blockData := msg.Payload[8:]
		n := copy(pieceBuf[begin:], blockData[:])
		return n, nil
	case MsgCancel:
		// flush internal buffer
		return 0, errors.New("received CANCEL")
	default:
		return 0, fmt.Errorf("unrecognized message id %v", msg.ID)
	}
	return 0, nil
}

// Bitfield communicates which pieces a peer has and can send us
type Bitfield []byte

func (b Bitfield) HasPiece(index int) bool {
	if len(b) == 0 {
		return false
	}

	byteIndex := index / 8
	offset := index % 8

	mask := 1 << (7 - offset)

	// fmt.Printf("Bitfield.HasPiece(%d), mask %b\n", index, mask)
	return (byte(mask) & b[byteIndex]) != 0
}

func (b Bitfield) SetPiece(index int) {
	byteIndex := index / 8
	offset := index % 8
	b[byteIndex] |= 1 << (7 - offset)
}
