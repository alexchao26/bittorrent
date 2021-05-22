// Package peer is a client implementation (TCP only) to peers in the swarm.
//
// The current implementation supports
//   - the extension protocol BEP0010
//   - metadata transfer between peers BEP0009
//   - DHT Port transfer BEP0005
package peer

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

type Client struct {
	conn              net.Conn // TCP connection to peer
	isChoked          bool
	bitfield          bitfield    // tracks which pieces the peer says it can send us
	peerID            [20]byte    // id reported from tracker in the original protocol
	supportsDHT       bool        // DHT protocol (BEP0005)
	addr              net.TCPAddr // stored for easy access to IP addr for DHT req
	dhtPort           int         // port for peer's DHT node
	supportsExtension bool        // extension protocol (BEP0010)
	metadataExtension struct {
		messageID    int // from ut_metadata in handshake
		metadataSize int
	}
}

// NewClient initializes a connection with a peer, then:
//   - completes the handshake
//   - completes the extension handshake if applicable
//   - receives the bitfield message
//   - sends an unchoke and interested message to the peer
func NewClient(addr net.TCPAddr, infoHash, peerID [20]byte) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr.String(), 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dialing: %w", err)
	}

	cli := &Client{
		conn:     conn,
		addr:     addr,
		isChoked: true,
	}

	cli.conn.SetDeadline(time.Now().Add(time.Second * 3))
	defer cli.conn.SetDeadline(time.Time{})

	err = cli.handshake(infoHash, peerID)
	if err != nil {
		return nil, fmt.Errorf("completing handshake: %w", err)
	}

	if cli.supportsExtension {
		cli.conn.SetDeadline(time.Now().Add(time.Second * 3))
		// receive extension message
		err = cli.receiveExtendedHandshake()
		if err != nil {
			return nil, fmt.Errorf("receiving extended handshake: %w", err)
		}
	}

	// receive bitfield message IF it hasn't been received already, some clients
	// will have sent it before the extended handshake
	if len(cli.bitfield) == 0 {
		cli.conn.SetDeadline(time.Now().Add(time.Second * 3))
		_, err = cli.receiveMessage()
		if err != nil {
			return nil, fmt.Errorf("receiving bitfield message: %w", err)
		}
		if len(cli.bitfield) == 0 {
			return nil, fmt.Errorf("bitfield not set")
		}
	}

	if cli.supportsDHT {
		cli.conn.SetDeadline(time.Now().Add(time.Second * 5))
		// allow 50 retries to account for clients that send other messages prematurely, the loop
		// will likely exit successfully or an i/o timeout on receiveMessage()
		for try := 0; try < 50 && cli.dhtPort == 0; try++ {
			_, err := cli.receiveMessage()
			if err != nil {
				// this shouldn't invalidate the peer connection altogether, so just break out
				break
			}
		}
	}

	cli.conn.SetDeadline(time.Now().Add(time.Second * 3))
	// send unchoke and interested message so the peer is ready for requests
	err = cli.sendMessage(msgUnchoke, nil)
	if err != nil {
		return nil, fmt.Errorf("sending unchoke: %w", err)
	}
	err = cli.sendMessage(msgInterested, nil)
	if err != nil {
		return nil, fmt.Errorf("sending interested: %w", err)
	}

	return cli, nil
}

// handshake completes the entire handshake process with the underlying peer
func (p *Client) handshake(infoHash, peerID [20]byte) error {
	const protocol = "BitTorrent protocol"
	var buf bytes.Buffer
	buf.WriteByte(byte(len(protocol)))
	buf.WriteString(protocol)

	extensionBytes := make([]byte, 8)
	// support BEP0010 Extension protocol
	// "20th bit from the right" = reserved_byte[5] & 0x10 (00010000 in binary)
	extensionBytes[5] |= 0x10
	extensionBytes[7] |= 1 // support BEP0005 DHT
	buf.Write(extensionBytes)

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

	read := lenProtocolStr
	var respExtensions [8]byte
	read += copy(respExtensions[:], handshakeBuf[read:read+8])
	if respExtensions[7]|1 != 0 {
		p.supportsDHT = true
	}
	// check if extension (BEP0010 protocol) byte is set
	if respExtensions[5]|0x10 != 0 {
		p.supportsExtension = true
	}

	var respInfoHash [20]byte
	read += copy(respInfoHash[:], handshakeBuf[read:read+20])
	copy(p.peerID[:], handshakeBuf[read:])

	if !bytes.Equal(respInfoHash[:], infoHash[:]) {
		return fmt.Errorf("infohashes do not match")
	}

	return nil
}

// Addr returns the address (IP and Port) of the remote peer.
func (p *Client) Addr() net.Addr {
	return p.conn.RemoteAddr()
}

// Close the underlying peer connection
func (p *Client) Close() error {
	return p.conn.Close()
}

// ErrNotInBitfield is returned on a call to *Client.GetPiece() if the peer does
// not have the requested piece.
var ErrNotInBitfield = errors.New("client does not have piece")

// GetPiece starts a download for the specified piece. If the returned error is
// non-nil and not ErrNotInBitfield, the peer can be considered "bad" and can be
// disconnected from
func (p *Client) GetPiece(index, length int, hash [20]byte) ([]byte, error) {
	if !p.bitfield.hasPiece(index) {
		// return a package level error so callers know not to disconnect from this peer
		return nil, ErrNotInBitfield
	}

	// set a deadline so a stuck peer relinquishes this job
	p.conn.SetDeadline(time.Now().Add(time.Second * 15))
	defer p.conn.SetDeadline(time.Time{})

	const maxBlockSize = 16384
	const maxBacklog = 10

	var requested, received, backlog int
	pieceBuf := make([]byte, length)
	for received < length {
		// create backlog
		for !p.isChoked && backlog < maxBacklog && requested < length {
			// request message format: <index, uint32><begin, uint32><request_size, uint32>
			// where begin is the offset for this piece, i.e. the total requested so far b/c
			// blocks are downloaded sequentially
			payload := make([]byte, 12)
			binary.BigEndian.PutUint32(payload[0:4], uint32(index))
			binary.BigEndian.PutUint32(payload[4:8], uint32(requested))
			// the final block may be truncated
			blockSize := maxBlockSize
			if requested+blockSize > length {
				blockSize = length - requested
			}
			binary.BigEndian.PutUint32(payload[8:12], uint32(blockSize))

			err := p.sendMessage(msgRequest, payload)
			if err != nil {
				return nil, fmt.Errorf("sending request message to create backlog: %w", err)
			}
			requested += blockSize
			backlog++
		}
		if p.isChoked {
			err := p.sendMessage(msgUnchoke, nil)
			if err != nil {
				return nil, fmt.Errorf("sending unchoke: %w", err)
			}
		}

		msg, err := p.receiveMessage()
		if err != nil {
			return nil, fmt.Errorf("receiving piece message from peer: %w", err)
		}
		if msg.id != msgPiece {
			continue
		}
		// piece format: <index, uint32><begin offset, uint32><data []byte>
		respIndex := binary.BigEndian.Uint32(msg.payload[0:4])
		if respIndex != uint32(index) {
			// possible for a client to send the "wrong" piece, just ignore it
			continue
		}

		begin := binary.BigEndian.Uint32(msg.payload[4:8])
		blockData := msg.payload[8:]
		// copy the block/data into the piece buffer
		n := copy(pieceBuf[begin:], blockData[:])

		// keep track of the number of received bytes and the backlog size
		received += n
		if n != 0 {
			backlog--
		}
	}

	// check integrity via SHA-1
	pieceHash := sha1.Sum(pieceBuf)
	if !bytes.Equal(pieceHash[:], hash[:]) {
		// disconnect from peer if they give us a bad piece
		return nil, fmt.Errorf("failed integrity check from %s", p.conn.RemoteAddr())
	}

	// tell peer we have this piece now
	havePayload := make([]byte, 4)
	binary.BigEndian.PutUint32(havePayload, uint32(index))
	p.sendMessage(msgHave, havePayload) // ignore errors

	return pieceBuf, nil
}

// DHTAddr returns the UDP address to reach this peer's DHT node. The peer
// should have sent a Port message after the BitTorrent handshake (per BEP0005).
// If it did not, a non-nil error is returned.
// http://bittorrent.org/beps/bep_0005.html
func (p *Client) DHTAddr() (net.UDPAddr, error) {
	if p.dhtPort == 0 {
		return net.UDPAddr{}, fmt.Errorf("did not provide DHT port")
	}
	return net.UDPAddr{
		IP:   p.addr.IP,
		Port: p.dhtPort,
	}, nil
}

// messageID are the types of messages that can be sent
// One drawback of using an iota is that msgChoke is the zero value. This
// doesn't cause any major issues in this project, but msgUnknown is provided as
// a drop-in for zero-value where needed.
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
	msgPort // 0x09 for supporting DHT (BEP0005)

	msgExtended messageID = 20 // BEP0010: Bittorrent protocol extension messages

	// additional message ids
	msgKeepAlive messageID = 254
	msgUnknown   messageID = 255 // provided for zero value in place of msgChoke
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
	msgPort:          "port",
	msgExtended:      "extended",
	msgKeepAlive:     "keep alive",
	msgUnknown:       "unknown",
}

func (m messageID) String() string {
	return messageIDStrings[m]
}

// sendMessage serializes and sends a message id and payload to the peer
func (p *Client) sendMessage(id messageID, payload []byte) error {
	length := uint32(len(payload) + 1) // +1 for ID
	message := make([]byte, length+4)  // + 4 to fit <length> at start of message
	binary.BigEndian.PutUint32(message[0:4], length)
	message[4] = byte(id)

	// add in payload if not a keep alive message
	if id != msgKeepAlive {
		copy(message[5:], payload)
	}

	_, err := p.conn.Write(message)
	if err != nil {
		return fmt.Errorf("writing message: %w", err)
	}

	return nil
}

type message struct {
	id      messageID
	payload []byte
}

// receiveMessage reads a message from the peer and processes it.
// Processing a message can change the state of the peer (choked/unchoked,
// its bitmap).
//
// The parsed message is also returned for further processing by the caller,
// e.g. for processing piece or extended metadata requests
func (p *Client) receiveMessage() (message, error) {
	// Receive and parse the message <length><id><payload>
	// 4 bytes that represent the length of the rest of the message
	lengthBuf := make([]byte, 4)
	_, err := io.ReadFull(p.conn, lengthBuf)
	if err != nil {
		return message{id: msgUnknown}, fmt.Errorf("reading message length: %w", err)
	}

	msgLength := binary.BigEndian.Uint32(lengthBuf)
	if msgLength == 0 {
		// keep-alive message
		return message{id: msgKeepAlive}, nil
	}

	// buffer to contain the rest of the message, 1 byte for the messageID, the
	// rest for the payload
	messageBuf := make([]byte, msgLength)
	_, err = io.ReadFull(p.conn, messageBuf)
	if err != nil {
		return message{id: msgUnknown}, fmt.Errorf("reading message payload: %w", err)
	}
	msgID := messageID(messageBuf[0])
	messagePayload := messageBuf[1:]

	// apply side effects to client if applicable
	switch msgID {
	case msgChoke:
		p.isChoked = true
	case msgUnchoke:
		p.isChoked = false
	case msgHave:
		index := binary.BigEndian.Uint32(messagePayload)
		p.bitfield.setPiece(int(index))
	case msgBitfield:
		p.bitfield = bitfield(messagePayload)
	case msgPort:
		p.dhtPort = int(binary.BigEndian.Uint16(messagePayload))
	}

	return message{
		id:      msgID,
		payload: messagePayload,
	}, nil
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
	// discard if index is out of range of bitfield
	if byteIndex >= len(b) {
		return
	}
	offset := index % 8
	mask := 1 << (7 - offset)
	b[byteIndex] |= byte(mask)
}
