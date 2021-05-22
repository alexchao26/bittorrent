// Package torrentfile parses torrent files or magnet links.
package torrentfile

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zeebo/bencode"
)

// Parse and return a TorrentFile that is a useful shape for actual downloading

// TorrentFile represents the contents of a .torrent file, reformatted for ease
// of use in the download process.
//
// The InfoHash is generated via SHA-1 from the entire Info field of the file.
//
// The 20-byte SHA1 hashes are formatted into a slice of 20-byte arrays for easy
// comparison with pieces downloaded from a peer.
type TorrentFile struct {
	TrackerURLs []string   // tracker URLs (from announce-list, announce or magnet link tr)
	InfoHash    [20]byte   // SHA-1 hash of the entire info field, uniquely identifies the torrent
	PieceHashes [][20]byte // SHA-1 hashes of each file piece
	PieceLength int        // number of bytes per piece
	Files       []File     // in the 1 file case, this will only have one element
	TotalLength int        // calculated as the sum of all files
	DisplayName string     // human readable display name (.torrent filename or magnet link dn)
}

// File contains metadata about the final downloaded files, namely their path
// and file length.
type File struct {
	Length   int    // length in bytes
	FullPath string // download path
	SHA1Hash string // optional for final validation
	MD5Hash  string // optional for final validation
}

// New returns a new TorrentFile.
//
// If the source is a .torrent file, it will be ready for use.
//
// If the source is a magnet link, metadata will need to be acquired from peers
// already in the swarm, then added using TorrentFile.AppendMetadata()
func New(source string) (TorrentFile, error) {
	if strings.HasSuffix(source, ".torrent") {
		return parseTorrentFile(source)
	}
	if strings.HasPrefix(source, "magnet") {
		return parseMagnetLink(source)
	}
	return TorrentFile{}, fmt.Errorf("invalid source (torrent file and magnet links supported)")
}

// parseTorrentFile parses a raw .torrent file.
func parseTorrentFile(filename string) (TorrentFile, error) {
	filename = os.ExpandEnv(filename)
	f, err := os.Open(filename)
	if err != nil {
		return TorrentFile{}, err
	}

	var btor bencodeTorrent
	err = bencode.NewDecoder(f).Decode(&btor)
	if err != nil {
		return TorrentFile{}, fmt.Errorf("unmarshalling file: %w", err)
	}

	var trackerURLs []string
	for _, list := range btor.AnnounceList {
		trackerURLs = append(trackerURLs, list...)
	}
	// BEP0012, only use `announce` if `announce-list` is not present
	if len(trackerURLs) == 0 {
		trackerURLs = append(trackerURLs, btor.Announce)
	}
	tf := TorrentFile{
		TrackerURLs: trackerURLs,
		DisplayName: filename,
	}

	err = tf.AppendMetadata(btor.Info)
	if err != nil {
		return TorrentFile{}, fmt.Errorf("parsing metadata: %w", err)
	}

	return tf, nil
}

// serialization struct the represents the structure of a .torrent file
// it is not immediately usable, so it can be converted to a TorrentFile struct
type bencodeTorrent struct {
	// URL of tracker server to get peers from
	Announce     string     `bencode:"announce"`
	AnnounceList [][]string `bencode:"announce-list"`
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

// AppendMetadata adds the metadata (aka the info dictionary of a torrent file).
// It must be called after torrentfile.New() is invoked with a magnet link
// source with the metadata acquired from a peer in the swarm.
func (t *TorrentFile) AppendMetadata(metadata []byte) error {
	var info bencodeInfo
	err := bencode.DecodeBytes(metadata, &info)
	if err != nil {
		return fmt.Errorf("unmarshalling info dict: %w", err)
	}

	// SHA-1 hash the entire info dictionary to get the info_hash
	t.InfoHash = sha1.Sum(metadata)

	// split the Pieces blob into the 20-byte SHA-1 hashes for comparison later
	const hashLen = 20 // length of a SHA-1 hash
	if len(info.Pieces)%hashLen != 0 {
		return errors.New("invalid length for info pieces")
	}
	t.PieceHashes = make([][20]byte, len(info.Pieces)/hashLen)
	for i := 0; i < len(t.PieceHashes); i++ {
		piece := info.Pieces[i*hashLen : (i+1)*hashLen]
		copy(t.PieceHashes[i][:], piece)
	}

	t.PieceLength = info.PieceLength

	// either Length OR Files field must be present (but not both)
	if info.Length == 0 && len(info.Files) == 0 {
		return fmt.Errorf("invalid torrent file info dict: no length OR files")
	}

	if info.Length != 0 {
		t.Files = append(t.Files, File{
			Length:   info.Length,
			FullPath: info.Name,
		})
		t.TotalLength = info.Length
	} else {
		for _, f := range info.Files {
			subPaths := append([]string{info.Name}, f.Path...)
			t.Files = append(t.Files, File{
				Length:   f.Length,
				FullPath: filepath.Join(subPaths...),
				SHA1Hash: f.SHA1Hash,
				MD5Hash:  f.MD5Hash,
			})
			t.TotalLength += f.Length
		}
	}

	return nil
}
