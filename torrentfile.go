package main

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/jackpal/bencode-go"
)

// Parse and return a TorrentFile that is a useful shape for actual downloading

// TorrentFile represents the contents of a .torrent file, reformatted for use
// in the actual download process.
//
// The InfoHash is generated via SHA-1 from the entire Info field of the file
//
// The 20 byte SHA1 hashes are formatted into a slice of 20-byte arrays for easy
// comparison with pieces downloaded from a peer
type TorrentFile struct {
	Announce string
	// SHA-1 hash of entire torrent file's Info field
	InfoHash [20]byte
	// individual hashes of each file piece
	PieceHashes [][20]byte
	// number of bytes(correct unit?) of each file piece
	PieceLength int
	Length      int
	Name        string
}

// ParseTorrentFile parses a torrent file via bencode.Unmarshal
func ParseTorrentFile(filename string) (TorrentFile, error) {
	fb, err := os.Open(os.ExpandEnv(filename))
	if err != nil {
		return TorrentFile{}, err
	}

	var bto bencodeTorrent
	err = bencode.Unmarshal(fb, &bto)
	if err != nil {
		return TorrentFile{}, fmt.Errorf("unmarshalling file %w", err)
	}

	tf, err := bto.toTorrentFile()
	if err != nil {
		return TorrentFile{}, fmt.Errorf("parsing file contents %w", err)
	}

	return tf, nil
}

// serialization struct the represents the structure of a .torrent file
// it is not immediately usable, so it can be converted to a TorrentFile struct
type bencodeTorrent struct {
	Announce string `bencode:"announce"` // URL of tracker server to get peers from
	Info     struct {
		Pieces      string `bencode:"pieces"`       // binary blob of all SHA1 hash of each piece
		PieceLength int    `bencode:"piece length"` // length (in bytes?) of each piece
		Length      int    `bencode:"length"`       // Length of file
		Name        string `bencode:"name"`         // Name of file
	} `bencode:"info"`
}

func (b bencodeTorrent) toTorrentFile() (TorrentFile, error) {
	// get info hash by bencode mashalling "info" field & SHA-1 hashing it
	var infoBuf bytes.Buffer
	err := bencode.Marshal(&infoBuf, b.Info)
	if err != nil {
		return TorrentFile{}, err
	}
	infoHash := sha1.Sum(infoBuf.Bytes())

	// break piece hashes into 20 byte pieces
	const hashLen = 20 // length of a SHA-1 hash

	// ensure evenly divisible by 20
	if len(b.Info.Pieces)%hashLen != 0 {
		return TorrentFile{}, errors.New("invalid length for info pieces")
	}
	pieceHashes := make([][20]byte, len(b.Info.Pieces)/hashLen)
	for i := 0; i < len(pieceHashes); i++ {
		piece := b.Info.Pieces[i*hashLen : (i+1)*hashLen]
		copy(pieceHashes[i][:], piece)
	}

	return TorrentFile{
		Announce:    b.Announce,
		InfoHash:    infoHash,
		PieceHashes: pieceHashes,
		PieceLength: b.Info.PieceLength,
		Length:      b.Info.Length,
		Name:        b.Info.Name,
	}, nil
}

func (t TorrentFile) BuildTrackerURL(peerID [20]byte, port int) (string, error) {
	u, err := url.Parse(t.Announce)
	if err != nil {
		return "", err
	}
	v := url.Values{}
	// hash of file we're downloading
	v.Add("info_hash", string(t.InfoHash[:]))
	// peer_id identifies ME, we're using some random. Real bittorrent
	// clients would identify client software and version
	v.Add("peer_id", string(peerID[:]))
	// port maybe should be a uint16?
	v.Add("port", strconv.Itoa(port))
	v.Add("uploaded", "0")
	v.Add("downloaded", "0")
	v.Add("compact", "1")
	v.Add("left", strconv.Itoa(t.Length))

	// set url query params
	u.RawQuery = v.Encode()

	return u.String(), nil
}
