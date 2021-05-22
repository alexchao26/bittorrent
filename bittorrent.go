// Package bittorrent implements the client side (downloads) of the BitTorrent
// protocol.
package bittorrent

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/alexchao26/bittorrent/peer"
	"github.com/alexchao26/bittorrent/torrentfile"
	"github.com/alexchao26/bittorrent/tracker"
)

type Download struct {
	torrent     torrentfile.TorrentFile
	peerID      [20]byte
	peerClients []*peer.Client
}

// NewDownload sets up a download given a source string (supports torrent file
// and magnet links. It attempts to get peer addresses from tracker servers,
// then gets metadata from peers if necessary (i.e. source is a magnet link).
// Once setup, the Download.Run() method will start the peer to peer download.
func NewDownload(source string) (*Download, error) {
	// parse off the intohash and tracker urls
	torrent, err := torrentfile.New(source)
	if err != nil {
		return nil, fmt.Errorf("parsing source: %w", err)
	}

	// peerID is generated randomly
	var peerID [20]byte
	rand.Read(peerID[:])
	// port doesn't matter for leeching
	port := 6881

	var peerAddrs []net.TCPAddr
	var wg sync.WaitGroup
	var mut sync.Mutex

	// get peer addresses from all trackers
	wg.Add(len(torrent.TrackerURLs))
	for _, trackerURL := range torrent.TrackerURLs {
		trackerURL := trackerURL
		go func() {
			defer wg.Done()
			addrs, err := tracker.GetPeers(trackerURL, torrent.InfoHash, peerID, port)
			if err != nil {
				fmt.Printf("error from tracker %s: %s\n", trackerURL, err.Error())
				return
			}
			mut.Lock()
			fmt.Printf("peers from %s: %d\n", trackerURL, len(addrs))
			peerAddrs = append(peerAddrs, addrs...)
			mut.Unlock()
		}()
	}
	wg.Wait()

	peerAddrs = dedupeAddrs(peerAddrs)

	// create all peer clients
	var peerClients []*peer.Client
	wg.Add(len(peerAddrs))
	for _, addr := range peerAddrs {
		addr := addr
		go func() {
			defer wg.Done()
			cli, err := peer.NewClient(addr, torrent.InfoHash, peerID)
			if err != nil {
				fmt.Printf("error connecting to peer at %s: %s\n", addr.String(), err.Error())
				return
			}
			mut.Lock()
			peerClients = append(peerClients, cli)
			mut.Unlock()
		}()
	}
	wg.Wait()

	fmt.Println("total peer count:", len(peerClients))

	if len(peerClients) == 0 {
		return nil, fmt.Errorf("failed to connect to any peers")
	}

	// get metadata if the source was a magnet link
	if strings.HasPrefix(source, "magnet") {
		var err error
		// getting metadata is fast enough that going to peers serially is fine
		var metadataBytes []byte
		for _, p := range peerClients {
			metadataBytes, err = p.GetMetadata(torrent.InfoHash)
			if err != nil {
				fmt.Printf("error getting metadata from %s: %s\n", p.Addr().String(), err.Error())
			}
			if err == nil {
				break
			}
		}

		if len(metadataBytes) == 0 {
			return nil, fmt.Errorf("failed getting metadata from all peers")
		}

		err = torrent.AppendMetadata(metadataBytes)
		if err != nil {
			return nil, fmt.Errorf("parsing metadata bytes: %w", err)
		}
	}

	return &Download{
		torrent:     torrent,
		peerClients: peerClients,
		peerID:      peerID,
	}, nil
}

// pieceJob includes metadata on a single piece to be downloaded
type pieceJob struct {
	index  int
	length int
	hash   [20]byte
}

// pieceResult contains the downloaded piece bytes and its index
type pieceResult struct {
	Index     int
	FilePiece []byte
}

// Run the peer to peer download process, concurrently getting pieces from each
// connected peer.
//
// The outDir defaults to the current directory, "./"
func (d *Download) Run(outDir string) error {
	if outDir == "" {
		outDir = "./"
	}
	// make job queue size the number of pieces, otherwise writing to an unbuffered channel will
	// block until "someone" reads that value
	jobQueue := make(chan pieceJob, len(d.torrent.PieceHashes))
	results := make(chan pieceResult)

	// start "worker" goroutine for each peer client to grab jobs off of the queue
	for _, p := range d.peerClients {
		p := p
		go func() {
			defer p.Close()
			for job := range jobQueue {
				pieceBuf, err := p.GetPiece(job.index, job.length, job.hash)
				if err != nil {
					// place job back on queue
					jobQueue <- job
					// if the client didn't have the piece, just continue along, don't close the
					// peer connection
					if errors.Is(err, peer.ErrNotInBitfield) {
						continue
					}
					// otherwise stop listening to jobQueue, defer will cleanup client connection
					fmt.Printf("disconnecting from %s after error: %s\n", p.Addr().String(), err.Error())
					return
				}
				results <- pieceResult{
					Index:     job.index,
					FilePiece: pieceBuf,
				}
			}
		}()
	}

	// send all jobs to jobQueue channel
	for i, hash := range d.torrent.PieceHashes {
		// all pieces are the full size except for the last piece
		length := d.torrent.PieceLength
		if i == len(d.torrent.PieceHashes)-1 {
			length = d.torrent.TotalLength - d.torrent.PieceLength*(len(d.torrent.PieceHashes)-1)
		}
		jobQueue <- pieceJob{
			index:  i,
			length: length,
			hash:   hash,
		}
	}

	// merge results into a final buffer
	resultBuf := make([]byte, d.torrent.TotalLength)
	for i := 0; i < len(d.torrent.PieceHashes); i++ {
		piece := <-results

		// copy to final buffer
		copy(resultBuf[piece.Index*d.torrent.PieceLength:], piece.FilePiece)
		fmt.Printf("%0.2f%% done\n", float64(i)/float64(len(d.torrent.PieceHashes))*100)
	}

	// close job queue which will close all peer connections
	close(jobQueue)

	// break resultBuf into separate files
	var usedBytes int
	for _, file := range d.torrent.Files {
		outPath := filepath.Join(outDir, file.FullPath)

		fmt.Printf("writing to file %q\n", outPath)
		// ensure the directory exists
		baseDir := filepath.Dir(outPath)
		_, err := os.Stat(baseDir)
		if os.IsNotExist(err) {
			err := os.MkdirAll(baseDir, os.ModePerm)
			if err != nil {
				return fmt.Errorf("making output directory: %w", err)
			}
		}

		// check integrity if hashes were provided in metadata
		fileRaw := resultBuf[usedBytes : usedBytes+file.Length]
		if file.SHA1Hash != "" {
			hash := sha1.Sum(fileRaw)
			if !bytes.Equal(hash[:], []byte(file.SHA1Hash)) {
				return fmt.Errorf("%q failed SHA-1 hash comparison", file.FullPath)
			}
		}
		if file.MD5Hash != "" {
			hash := md5.Sum(fileRaw)
			if !bytes.Equal(hash[:], []byte(file.MD5Hash)) {
				return fmt.Errorf("%q failed MD5 hash comparison", file.FullPath)
			}
		}

		// write to the file
		err = os.WriteFile(outPath, fileRaw, os.ModePerm)
		usedBytes += file.Length
		if err != nil {
			return fmt.Errorf("writing to file: %w", err)
		}
	}
	return nil
}

// dedupeAddrs is a helper function to dedupe all the peer addresses received
// from multiple tracker servers
func dedupeAddrs(addrs []net.TCPAddr) []net.TCPAddr {
	deduped := []net.TCPAddr{}
	set := map[string]bool{}
	for _, a := range addrs {
		if !set[a.String()] {
			deduped = append(deduped, a)
			set[a.String()] = true
		}
	}
	return deduped
}
