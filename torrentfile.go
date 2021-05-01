package main

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/zeebo/bencode"
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
	Announce    string     // a url to get peers from
	InfoHash    [20]byte   // SHA-1 hash of entire torrent file's Info field
	PieceHashes [][20]byte // individual SHA-1 hashes of each file piece
	PieceLength int        // number of bytes of each piece
	TotalLength int        // Calculated as the sum of all files
	Files       []File     // in the 1 file case, this will only have one element
}

type File struct {
	Length   int      // length in bytes
	FullPath string   // download path
	SHA1Hash [20]byte // optional for final validation
	MD5Hash  [20]byte // optional for final validation
}

// ParseTorrentFile parses a raw .torrent file into a structure that aligns
// with the peer to peer download process.
func ParseTorrentFile(filename string) (TorrentFile, error) {
	f, err := os.Open(os.ExpandEnv(filename))
	if err != nil {
		return TorrentFile{}, err
	}

	var btor bencodeTorrent
	err = bencode.NewDecoder(f).Decode(&btor)
	if err != nil {
		return TorrentFile{}, fmt.Errorf("unmarshalling file: %w", err)
	}

	var info bencodeInfo
	err = bencode.DecodeBytes(btor.Info, &info)
	if err != nil {
		return TorrentFile{}, fmt.Errorf("umarshalling info dict: %w", err)
	}

	tf, err := toTorrentFile(btor, info)
	if err != nil {
		return TorrentFile{}, fmt.Errorf("parsing file contents: %w", err)
	}

	return tf, nil
}

func (t TorrentFile) BuildTrackerURL(peerID [20]byte, port int) (string, error) {
	u, err := url.Parse(t.Announce)
	if err != nil {
		return "", err
	}
	v := url.Values{}
	v.Add("info_hash", string(t.InfoHash[:]))
	// my peer_id (just random). Real bittorrent clients would identify software and version
	v.Add("peer_id", string(peerID[:]))
	v.Add("port", strconv.Itoa(port))
	v.Add("uploaded", "0")
	v.Add("downloaded", "0")
	v.Add("compact", "1") // BEP0023: compact peer list
	v.Add("left", strconv.Itoa(t.TotalLength))

	// set url query params
	u.RawQuery = v.Encode()

	return u.String(), nil
}

// serialization struct the represents the structure of a .torrent file
// it is not immediately usable, so it can be converted to a TorrentFile struct
type bencodeTorrent struct {
	// URL of tracker server to get peers from
	Announce string `bencode:"announce"`
	// Info is parsed as a RawMessage to ensure that the final info_hash is
	// correct even in the case of the info dictionary being an unexpected shape
	Info bencode.RawMessage `bencode:"info"`
}

// Only Length OR Files will be present per BEP0003
// spec: http://bittorrent.org/beps/bep_0003.html#info-dictionary
type bencodeInfo struct {
	Pieces      string `bencode:"pieces"`       // binary blob of all SHA1 hash of each piece
	PieceLength int    `bencode:"piece length"` // length in bytes of each piece
	Name        string `bencode:"name"`         // Name of file (or folder if there are multiple files)
	Length      int    `bencode:"length"`       // total length of file (in single file case)
	Files       []struct {
		Length   int      `bencode:"length"` // length of this file
		Path     []string `bencode:"path"`   // list of subdirectories, last element is file name
		SHA1Hash string   `bencode:"sha1"`   // optional, to validate this file
		MD5Hash  string   `bencode:"md5"`    // optional, to validate this file
	} `bencode:"files"`
}

func toTorrentFile(btor bencodeTorrent, info bencodeInfo) (TorrentFile, error) {
	// SHA-1 hash the entire info dictionary to get the info_hash
	infoHash := sha1.Sum(btor.Info)

	// split the Pieces blob into the 20-byte SHA-1 hashes for comparison later
	const hashLen = 20 // length of a SHA-1 hash
	if len(info.Pieces)%hashLen != 0 {
		return TorrentFile{}, errors.New("invalid length for info pieces")
	}
	pieceHashes := make([][20]byte, len(info.Pieces)/hashLen)
	for i := 0; i < len(pieceHashes); i++ {
		piece := info.Pieces[i*hashLen : (i+1)*hashLen]
		copy(pieceHashes[i][:], piece)
	}

	// either Length OR Files field must be present (but not both)
	if info.Length == 0 && len(info.Files) == 0 {
		return TorrentFile{}, fmt.Errorf("invalid torrent file info dict: no length OR files")
	}

	var files []File
	var totalLength int
	if info.Length != 0 {
		files = append(files, File{
			Length:   info.Length,
			FullPath: info.Name,
		})
		totalLength = info.Length
	} else {
		for _, f := range info.Files {
			subPaths := append([]string{info.Name}, f.Path...)
			file := File{
				Length:   f.Length,
				FullPath: filepath.Join(subPaths...),
			}
			copy(file.SHA1Hash[:], []byte(f.SHA1Hash))
			copy(file.MD5Hash[:], []byte(f.MD5Hash))
			files = append(files, file)
			totalLength += f.Length
		}
	}

	return TorrentFile{
		Announce:    btor.Announce,
		InfoHash:    infoHash,
		PieceHashes: pieceHashes,
		PieceLength: info.PieceLength,
		TotalLength: totalLength,
		Files:       files,
	}, nil
}
