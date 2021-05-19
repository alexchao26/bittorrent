package main

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"time"

	"github.com/zeebo/bencode"
)

// Implementation of extensions to the BitTorrent Protocol.
// http://www.bittorrent.org/beps/bep_0010.html

type extendedHandshakeMessage struct {
	// Top level dictionary key "m"
	M struct {
		// Value doubles as the extended message ID for metadata requests
		Metadata int `bencode:"ut_metadata"`
	} `bencode:"m"`
	MetadataSize int `bencode:"metadata_size"`
}

func (p *PeerClient) receiveExtendedHandshake() error {
	// <length, uint32><message_id (20), uint8><extended_msg_id, uint8><msg []byte>
	msg, err := p.receiveMessage()
	if err != nil {
		return fmt.Errorf("reading extension handshake length: %w", err)
	}

	// Allow 50 read retries if a non-extended message is received. Some clients will send
	// bitfield, unchoke, or have before the extended handshake message which doesn't need to be
	// handled as an error.
	for i := 0; i < 50 && messageID(msg.id) != msgExtended; i++ {
		lastMsgID := messageID(msg.id)
		msg, err = p.receiveMessage()
		if err != nil {
			return fmt.Errorf("retrying extended handshake after %s message: %w", lastMsgID, err)
		}
	}

	if messageID(msg.id) != msgExtended {
		return fmt.Errorf("expected extended message, got %s", messageID(msg.id))
	}

	const extendedHandshakeID uint8 = 0
	extMsgID := uint8(msg.payload[0])
	payload := msg.payload[1:]

	if extMsgID != extendedHandshakeID {
		return fmt.Errorf("expected extended handshake id (0), got %d", extMsgID)
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

type metadataMessageType uint8

const (
	request metadataMessageType = iota
	data
	reject
)

type metadataMessage struct {
	Type      metadataMessageType `bencode:"msg_type"`
	Piece     int                 `bencode:"piece"`
	TotalSize int                 `bencode:"total_size,omitempty"`
}

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

			msgRaw, err := bencode.EncodeBytes(metadataMessage{
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

		var msgResp metadataMessage
		err = bencode.DecodeBytes(dictRaw, &msgResp)
		if err != nil {
			return nil, fmt.Errorf("decoding bencoded extension dictionary: %w", err)
		}

		if msgResp.Type == reject {
			return nil, fmt.Errorf("got metadata reject message for piece %d", msgResp.Piece)
		}
		if msgResp.Type != data {
			return nil, fmt.Errorf("want data type (1), got %d", msgResp.Type)
		}
		if msgResp.TotalSize != p.metadataExtension.metadataSize {
			return nil, fmt.Errorf("got metadata data.total_size %d, want %d", msgResp.TotalSize, p.metadataExtension.metadataSize)
		}

		// piece bytes are after the dictionary
		pieceRaw := extPayload[len(dictRaw):]

		// copy into metadata buffer & update the number of received bytes
		received += copy(metadataBuf[msgResp.Piece*metadataPieceSize:], pieceRaw[:])
	}

	// validate metadata via SHA-1
	hash := sha1.Sum(metadataBuf)
	if !bytes.Equal(hash[:], infoHash[:]) {
		return nil, fmt.Errorf("metadata failed integrity check")
	}
	return metadataBuf, nil
}
