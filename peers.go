package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

type PeerClient struct {
	conn     net.Conn // TCP connection to peer
	isChoked bool
	bitfield bitfield // tracks which pieces the peer says it can send us
	peerID   [20]byte // id reported from tracker in the original protocol
}

// NewPeerClient initializes a connection with a peer, completing the handshake
func NewPeerClient(peerAddr net.TCPAddr, infoHash, peerID [20]byte) (*PeerClient, error) {
	conn, err := net.DialTimeout("tcp", peerAddr.String(), 3*time.Second)
	if err != nil {
		return nil, err
	}

	cli := &PeerClient{
		conn: conn,
	}

	err = cli.handshake(infoHash, peerID)
	if err != nil {
		return nil, err
	}

	// receive bitfield message
	_, err = cli.receiveMessage(nil)
	if err != nil {
		return nil, err
	}

	return cli, nil
}

// Attempt to handshake with the underlying peer
func (p *PeerClient) handshake(infoHash, peerID [20]byte) error {
	const protocol = "BitTorrent protocol"
	var buf bytes.Buffer
	buf.WriteByte(byte(len(protocol)))
	buf.WriteString(protocol)
	buf.Write(make([]byte, 8))
	buf.Write(infoHash[:])
	buf.Write(peerID[:])

	_, err := p.conn.Write(buf.Bytes())
	if err != nil {
		return fmt.Errorf("sending handshake message to %s: %w", p.conn.RemoteAddr(), err)
	}

	// read handshake message
	lengthBuf := make([]byte, 1)
	_, err = io.ReadFull(p.conn, lengthBuf)
	if err != nil {
		return err
	}
	lenProtocolStr := int(lengthBuf[0])
	if lenProtocolStr != 19 {
		return fmt.Errorf("reading handshake, protocol length is not 19, got %d", lenProtocolStr)
	}

	handshakeBuf := make([]byte, lenProtocolStr+48)
	_, err = io.ReadFull(p.conn, handshakeBuf)
	if err != nil {
		return fmt.Errorf("reading handshake message: %w", err)
	}

	// parse handshake details into handshake
	respProtocol := string(handshakeBuf[:lenProtocolStr])
	if respProtocol != protocol {
		return fmt.Errorf("expected %q protocol buffer, got %q", protocol, respProtocol)
	}
	curr := lenProtocolStr
	// todo update 8 extension support bytes when adding BEP0009
	curr += 8 // ignore 8 bytes for supporting extensions
	var respInfoHash [20]byte
	copy(respInfoHash[:], handshakeBuf[curr:curr+20])
	copy(p.peerID[:], handshakeBuf[curr+20:])

	if !bytes.Equal(respInfoHash[:], infoHash[:]) {
		return fmt.Errorf("expected infohash %v but got %v", infoHash, respInfoHash)
	}

	return nil
}

// Job contains info needed to download a piece from a peer
type Job struct {
	Index  int
	Length int
	Hash   [20]byte
}

// Piece contains info to place a downloaded piece into its final location
// amongst the downloaded file(s), namely the piece itself and its index
type Piece struct {
	Index     int
	FilePiece []byte
}

func (p *PeerClient) ListenForJobs(jobQueue chan *Job, results chan<- *Piece) error {
	defer p.conn.Close()

	p.conn.SetDeadline(time.Now().Add(time.Second * 5))

	// begin by sending unchoke and interested
	err := p.sendMessage(msgUnchoke, nil)
	if err != nil {
		return fmt.Errorf("sending unchoke to %s: %w", p.conn.RemoteAddr(), err)
	}
	err = p.sendMessage(msgInterested, nil)
	if err != nil {
		return fmt.Errorf("sending interested to %s: %w", p.conn.RemoteAddr(), err)
	}

	// receive jobs off the job queue channel
	// in any situation where the peer cannot or fails to send us a piece, return the job to the
	// queue (and in likely unrecoverable circumstances, disconnect from the peer)
	for job := range jobQueue {
		if !p.bitfield.hasPiece(job.Index) {
			jobQueue <- job
			continue
		}

		var requested, received, backlog int
		pieceBuf := make([]byte, job.Length)

		// set a deadline so a stuck client puts its job back on the queue
		p.conn.SetDeadline(time.Now().Add(time.Second * 30))

		const maxBlockSize = 16384
		// how many unfulfilled requests to have at one time
		const maxBacklog = 10

		for received < job.Length {
			// create backlog
			for !p.isChoked && backlog < maxBacklog && requested < job.Length {
				// request message format: <index, uint32><begin, uint32><request_size, uint32>
				// where begin is the offset for this piece, i.e. the total requested so far b/c
				// blocks are downloaded sequentially
				payload := make([]byte, 12)
				binary.BigEndian.PutUint32(payload[0:4], uint32(job.Index))
				binary.BigEndian.PutUint32(payload[4:8], uint32(requested))
				// the final block may be truncated
				blockSize := maxBlockSize
				if requested+blockSize > job.Length {
					blockSize = job.Length - requested
				}
				binary.BigEndian.PutUint32(payload[8:12], uint32(blockSize))

				p.sendMessage(msgRequest, payload)
				requested += blockSize
				backlog++
			}
			if p.isChoked {
				err := p.sendMessage(msgUnchoke, nil)
				if err != nil {
					return fmt.Errorf("sending unchoke to %s: %w", p.conn.RemoteAddr(), err)
				}
			}

			n, err := p.receiveMessage(pieceBuf)
			if err != nil {
				// if there was an error, kill this peer
				jobQueue <- job
				return fmt.Errorf("receiving message from %s: %w", p.conn.RemoteAddr(), err)
			}

			// keep track of the number of received bytes and the backlog size
			received += n
			if n != 0 {
				backlog--
			}
		}

		// check integrity via SHA-1
		pieceHash := sha1.Sum(pieceBuf)
		if !bytes.Equal(pieceHash[:], job.Hash[:]) {
			jobQueue <- job
			// disconnect from peer if they give us a bad piece
			return fmt.Errorf("failed integrity check from %s", p.conn.RemoteAddr())
		}

		// tell peer we HAVE this piece now
		// TODO this should be relayed to ALL peers, might require a mutex for p.conn.Write?
		havePayload := make([]byte, 4)
		binary.BigEndian.PutUint32(havePayload, uint32(job.Index))
		p.sendMessage(msgHave, havePayload) // ignore errors

		// write to the results channel for processing
		results <- &Piece{
			Index:     job.Index,
			FilePiece: pieceBuf,
		}
	}

	return nil
}

// messageID are the types of messages that can be sent
type messageID uint8

const (
	msgChoke messageID = iota
	msgUnchoke
	msgInterested
	msgNotInterested
	msgHave
	msgBitfield
	msgRequest
	msgPiece
	msgCancel
)

var messageIDStrings = map[messageID]string{
	msgChoke:         "choke",
	msgUnchoke:       "unchoke",
	msgInterested:    "interested",
	msgNotInterested: "not interested",
	msgHave:          "have",
	msgBitfield:      "bitfield",
	msgRequest:       "request",
	msgPiece:         "piece",
	msgCancel:        "cancel",
}

func (m messageID) String() string {
	return messageIDStrings[m]
}

// sendMessage serializes and sends a message id and payload to the peer
//
// Note: KeepAlive messages are not implemented
func (p *PeerClient) sendMessage(id messageID, payload []byte) error {
	length := uint32(len(payload) + 1) // +1 for ID
	message := make([]byte, length+4)  // + 4 to fit <length> at start of message
	binary.BigEndian.PutUint32(message[0:4], length)
	message[4] = byte(id)
	copy(message[5:], payload)

	_, err := p.conn.Write(message)
	if err != nil {
		return fmt.Errorf("writing message: %w", err)
	}

	return nil
}

// receiveMessage reads a message from the peer and processes it.
// Processing a message can change the state of the peer (choked/unchoked,
// its bitmap) and the state of the piece that is currently being downloaded
// into the pieceBuffer.
//
// The number of bytes processed is returned as the first variable to indicate
// when a piece is received, as opposed to a different Message type.
func (p *PeerClient) receiveMessage(pieceBuffer []byte) (int, error) {
	// Receive and parse the message <length><id><payload>
	// 4 bytes that represent the length of the rest of the message
	lengthBuf := make([]byte, 4)
	_, err := io.ReadFull(p.conn, lengthBuf)
	if err != nil {
		return 0, fmt.Errorf("reading message length: %w", err)
	}

	messageLength := binary.BigEndian.Uint32(lengthBuf)
	if messageLength == 0 {
		// keep-alive message
		return 0, nil
	}

	// buffer to contain the rest of the message, 1 byte for the messageID, the
	// rest for the payload
	messageBuf := make([]byte, messageLength)
	_, err = io.ReadFull(p.conn, messageBuf)
	if err != nil {
		return 0, fmt.Errorf("reading message payload: %w", err)
	}
	messageID := messageID(messageBuf[0])
	messagePayload := messageBuf[1:]

	// Process the message
	switch messageID {
	case msgChoke:
		p.isChoked = true
	case msgUnchoke:
		p.isChoked = false
	case msgInterested:
		// not implemented, purely leeching
	case msgNotInterested:
		// not implemented, purely leeching
	case msgHave:
		index := binary.BigEndian.Uint32(messagePayload)
		p.bitfield.setPiece(int(index))
	case msgBitfield:
		p.bitfield = messagePayload
	case msgRequest:
		// not implemented, purely leeching
	case msgPiece:
		// piece format: <index, uint32><begin offset, uint32><data []byte>
		index := binary.BigEndian.Uint32(messagePayload[0:4])
		_ = index // unused b/c accessible via Job struct

		// should error handle against the job's index?
		begin := binary.BigEndian.Uint32(messagePayload[4:8])
		blockData := messagePayload[8:]
		// copy the block/data into the piece buffer
		n := copy(pieceBuffer[begin:], blockData[:])
		return n, nil
	case msgCancel:
		// a leech shouldn't receive a cancel message, so error out here
		return 0, errors.New("received CANCEL")
	default:
		return 0, fmt.Errorf("received unrecognized message type: %s", messageID.String())
	}

	return 0, nil
}

// bitfield communicates which pieces a peer has and can send us
type bitfield []byte

// hasPiece checks the bitfield to see if that piece is available
func (b bitfield) hasPiece(index int) bool {
	if len(b) == 0 {
		return false
	}

	byteIndex := index / 8
	offset := index % 8

	mask := 1 << (7 - offset)

	return (byte(mask) & b[byteIndex]) != 0
}

// setPiece updates the bitfield to indicate that the peer can send that piece
func (b bitfield) setPiece(index int) {
	byteIndex := index / 8
	// shield against the bitfield being uninitialized
	if len(b) <= index {
		b = make([]byte, index)
	}
	offset := index % 8
	mask := 1 << (7 - offset)
	b[byteIndex] |= byte(mask)
}
