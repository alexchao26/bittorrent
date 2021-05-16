package main

import (
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

// todo implement a readMetadata message which can switch onto a metadataReject?
// type metadataReject struct {
// 	Type  int `bencode:"msg_type"`
// 	Piece int `bencode:"piece"`
// }

func (p *PeerClient) GetMetadata(infoHash [20]byte) ([]byte, error) {
	if p.metadataExtension.messageID == 0 || p.metadataExtension.metadataSize == 0 {
		return nil, fmt.Errorf("client does not support metadata extension")
	}
	defer p.conn.SetDeadline(time.Time{})

	metadataBuf := make([]byte, p.metadataExtension.metadataSize)

	const metadataPieceSize = 16384 // 16KiB

	var requested, received int
	for received < p.metadataExtension.metadataSize {
		// request one piece at a time, in my experience, clients don't like backlogging/pipelining
		// metadata piece requests  and they'll end up sending the first piece only
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
				return nil, fmt.Errorf("sending metadata request: %w", err)
			}
			requested++
		}

		// update deadline for reading each piece
		p.conn.SetDeadline(time.Now().Add(time.Second * 5))
		msg, err := p.receiveMessage()
		if err != nil {
			return nil, fmt.Errorf("receiving metadata piece: %w", err)
		}
		// clients will send non-extended messages, like unchoke/have, ignore them
		if msg.id != msgExtended {
			continue
		}

		// process the extended message http://www.bittorrent.org/beps/bep_0010.html
		extMsgID := uint8(msg.payload[0])
		// expect extension message id to match, although in practice it's always been zero
		_ = extMsgID
		// if int(extMsgID) != p.metadataExtension.messageID {
		// 	return fmt.Errorf("metadata extension ids do not match: want %d, got %d", p.metadataExtension.messageID, extMsgID)
		// }

		extPayload := msg.payload[1:]
		// read bytes until end of bencoded dictionary ("ee" ends total_size integer then dictionary)
		var dictRaw []byte
		for i := 1; i < len(extPayload); i++ {
			if string(extPayload[i-1:i+1]) == "ee" {
				dictRaw = extPayload[:i+1]
				break
			}
		}
		if len(dictRaw) == 0 {
			return nil, fmt.Errorf("malformed extension dictionary")
		}

		var data metadataData
		err = bencode.DecodeBytes(dictRaw, &data)
		if err != nil {
			return nil, fmt.Errorf("decoding bencoded extension dictionary: %w", err)
		}

		if data.Type != 1 {
			return nil, fmt.Errorf("expected data type (1), got %d", data.Type)
		}
		if data.TotalSize != p.metadataExtension.metadataSize {
			return nil, fmt.Errorf("got metadata data.total_size %d, want %d", data.TotalSize, p.metadataExtension.metadataSize)
		}

		// piece bytes are after the dictionary
		pieceRaw := extPayload[len(dictRaw):]

		// copy into metadata buffer & update the number of received bytes
		received += copy(metadataBuf[data.Piece*metadataPieceSize:], pieceRaw[:])
	}

	// validate metadata via SHA-1
	hash := sha1.Sum(metadataBuf)
	if !bytes.Equal(hash[:], infoHash[:]) {
		return nil, fmt.Errorf("metadata failed integrity check")
	}
	return metadataBuf, nil
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

	p.metadataExtension.messageID = extendedResp.M.Metadata
	p.metadataExtension.metadataSize = extendedResp.MetadataSize

	return nil
}
