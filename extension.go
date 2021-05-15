package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/zeebo/bencode"
)

// Implementation of extensions to the BitTorrent Protocol.

type metadataExtensionType uint8

const (
	request metadataExtensionType = iota
	data
	reject
)

type metadataRequest struct {
	Type  metadataExtensionType `bencode:"msg_type"`
	Piece int                   `bencode:"piece"`
}

// todo fix struct name
type metadataData struct {
	Type      metadataExtensionType `bencode:"msg_type"`
	Piece     int                   `bencode:"piece"`
	TotalSize int                   `bencode:"total_size"`
}

// currently unused
type metadataReject struct {
	Type  int `bencode:"msg_type"`
	Piece int `bencode:"piece"`
}

const metadataPieceSize = 16384 // 16KiB

func (p *PeerClient) GetMetadata(infoHash [20]byte) ([]byte, error) {
	if p.metadataExtension.messageID == 0 || p.metadataExtension.metadataSize == 0 {
		return nil, fmt.Errorf("client (%s) does not support metadata extension", p.conn.RemoteAddr())
	}
	defer p.conn.SetDeadline(time.Time{})

	// send unchoke and intersted
	err := p.sendMessage(msgUnchoke, nil)
	if err != nil {
		return nil, fmt.Errorf("sending unchoke: %w", err)
	}
	err = p.sendMessage(msgInterested, nil)
	if err != nil {
		return nil, fmt.Errorf("sending interetsed: %w", err)
	}

	p.rawMetadata = make([]byte, p.metadataExtension.metadataSize)

	// // calculate number of pieces
	// pieces := int(math.Ceil(float64(p.metadataExtension.metadataSize) / float64(metadataPieceSize)))

	// // send all piece requests upfront, one peer should be able to handle it
	// for i := 0; i < pieces; i++ {
	// 	var buf bytes.Buffer

	// 	// write the message id for the extension protocol
	// 	buf.WriteByte(byte(p.metadataExtension.messageID))

	// 	msgRaw, err := bencode.EncodeBytes(metadataRequest{
	// 		Type:  request,
	// 		Piece: i,
	// 	})
	// 	if err != nil {
	// 		return nil, fmt.Errorf("bencoding metadata req: %w", err)
	// 	}
	// 	buf.Write(msgRaw)

	// 	p.conn.SetDeadline(time.Now().Add(time.Second * 3))
	// 	err = p.sendMessage(msgExtended, buf.Bytes())
	// 	if err != nil {
	// 		return nil, fmt.Errorf("sending metadata piece request: %w", err)
	// 	}
	// }

	var requested int
	var received int
	for received < p.metadataExtension.metadataSize {
		// do not build up a backlog/TCP pipeline for metadata requests
		// in my experience that will cause issues
		if requested <= received/metadataPieceSize {
			var buf bytes.Buffer

			// write the message id for the extension protocol
			buf.WriteByte(byte(p.metadataExtension.messageID))

			msgRaw, err := bencode.EncodeBytes(metadataRequest{
				Type:  request,
				Piece: requested,
			})
			if err != nil {
				return nil, fmt.Errorf("bencoding metadata req: %w", err)
			}
			buf.Write(msgRaw)

			p.conn.SetDeadline(time.Now().Add(time.Second * 3))
			err = p.sendMessage(msgExtended, buf.Bytes())
			if err != nil {
				return nil, fmt.Errorf("sending metadata piece request: %w", err)
			}
			requested++
		}
		// update deadline for reading each piece
		// long ass deadline for reading each piece
		p.conn.SetDeadline(time.Now().Add(time.Second * 15))
		_, n, err := p.receiveMessage(nil)
		if err != nil {
			return nil, fmt.Errorf("attempting to get metadata piece: %w", err)
		}
		received += n
	}

	// validate metadata via SHA-1
	hash := sha1.Sum(p.rawMetadata)
	if !bytes.Equal(hash[:], infoHash[:]) {
		return nil, fmt.Errorf("metadata failed integrity check from %s", p.conn.RemoteAddr())
	}
	return p.rawMetadata, nil
}

func (p *PeerClient) receiveExtendedHandshake() error {
	// <length, uint32><message_id (20), uint8><extended_msg_id, uint8><msg []byte>
	lengthRaw := make([]byte, 4)
	_, err := io.ReadFull(p.conn, lengthRaw)
	if err != nil {
		return fmt.Errorf("reading extension handshake length: %w", err)
	}
	length := binary.BigEndian.Uint32(lengthRaw)

	messagePayload := make([]byte, length)
	_, err = io.ReadFull(p.conn, messagePayload)
	if err != nil {
		return fmt.Errorf("reading extension handshake payload: %w", err)
	}

	msgID := messagePayload[0]
	if messageID(msgID) != msgExtended {
		return fmt.Errorf("expected extended message, got %s", messageID(msgID))
	}

	const extendedHandshake uint8 = 0
	extMsgID := uint8(messagePayload[1])
	payload := messagePayload[2:]

	if extMsgID != extendedHandshake {
		return fmt.Errorf("expected extended handshake id (0), got %d", extMsgID)
	}

	type extendedHandshakeMessage struct {
		// Top level dictionary key "m"
		M struct {
			// Value doubles as the extended message ID for metadata requests
			Metadata int `bencode:"ut_metadata"`
		} `bencode:"m"`
		MetadataSize int `bencode:"metadata_size"`
	}
	var extendedResp extendedHandshakeMessage
	err = bencode.DecodeBytes(payload, &extendedResp)
	if err != nil {
		return fmt.Errorf("decoding extended handshake msg: %w", err)
	}

	// fmt.Printf("\textended handhshake msg: %+v\n", extendedResp)

	p.metadataExtension.messageID = extendedResp.M.Metadata
	p.metadataExtension.metadataSize = extendedResp.MetadataSize

	fmt.Printf("DONE with extended handshake with %s\n", p.conn.RemoteAddr())

	return nil
}

// http://www.bittorrent.org/beps/bep_0010.html
func (p *PeerClient) processExtendedMessage(messagePayload []byte) (int, error) {
	extMsgID := uint8(messagePayload[0])
	payload := messagePayload[1:]
	fmt.Println("extended message ID is: ", extMsgID)

	// TODO this is behaving unexpectedly
	// // expect extension message id to match
	fmt.Println("msg ids misbehaving??", extMsgID, p.metadataExtension.messageID)
	// if int(extMsgID) != p.metadataExtension.messageID {
	// 	return fmt.Errorf("metadata extension ids do not match: want %d, got %d", p.metadataExtension.messageID, extMsgID)
	// }

	// length of the message dictionary is the entire length minus:
	// 1 for pieceLength byte and length of piece itself

	dictRaw := []byte{}
	bufReader := bufio.NewReader(bytes.NewReader(payload))
	for {
		b, err := bufReader.ReadByte()
		if err != nil {
			return 0, fmt.Errorf("reading extension dictionary byte: %w", err)
		}
		dictRaw = append(dictRaw, b)
		if l := len(dictRaw); l > 2 && string(dictRaw[l-2:]) == "ee" {
			break
		}
	}

	fmt.Println("dictionary bytes as string...", string(dictRaw), p.conn.RemoteAddr())

	var data metadataData
	err := bencode.DecodeBytes(dictRaw, &data)
	if err != nil {
		return 0, fmt.Errorf("decoding bencoded extension dictionary: %w", err)
	}
	msgType := data.Type
	piece := data.Piece
	// totalSize := data.TotalSize // unused

	if msgType != 1 {
		return 0, fmt.Errorf("expected data type (1), got %d", msgType)
	}

	// piece bytes are after the dictionary
	pieceRaw := payload[len(dictRaw):]

	copy(p.rawMetadata[piece*metadataPieceSize:], pieceRaw[:])
	return len(pieceRaw), nil
}
