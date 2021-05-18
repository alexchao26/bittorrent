package main

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

func main() {
	source := flag.String("source", "", "torrent file or magnet link (required)")
	outDir := flag.String("outdir", "./", "output directory")
	maxOpenFiles := flag.Uint64("maxOpenFiles", 1024*8, "max number of file descriptors")
	flag.Parse()
	if *source == "" {
		panic("source flag is required (torrent file or magnet link)")
	}

	// set file descriptors 'ulimit -n $FILES'
	rLimit := syscall.Rlimit{
		Cur: *maxOpenFiles,
		Max: *maxOpenFiles,
	}
	err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		panic("updating rlimit: " + err.Error())
	}

	var torrent TorrentFile
	var infoHash [20]byte
	var trackerURLs []string
	// parse off the intohash and tracker urls
	if strings.HasPrefix(*source, "magnet") {
		magnetLink, err := ParseMagnetLink(*source)
		if err != nil {
			panic("error parsing magnet link: " + err.Error())
		}
		trackerURLs = magnetLink.TrackerURLs
		infoHash = magnetLink.InfoHash
	} else if strings.HasSuffix(*source, ".torrent") {
		var err error
		torrent, err = ParseTorrentFile(*source)
		if err != nil {
			panic(err)
		}

		trackerURLs = torrent.TrackerURLs
		infoHash = torrent.InfoHash
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
	wg.Add(len(trackerURLs))
	for _, trackerURL := range trackerURLs {
		trackerURL := trackerURL
		go func() {
			defer wg.Done()
			addrs, err := GetPeersFromTracker(trackerURL, infoHash, peerID, port)
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

	peerAddrs = DedupeAddrs(peerAddrs)

	// create all peer clients
	var peerClients []*PeerClient
	wg.Add(len(peerAddrs))
	for _, addr := range peerAddrs {
		addr := addr
		go func() {
			defer wg.Done()
			cli, err := NewPeerClient(addr, infoHash, peerID)
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

	// get metadata if the source was a magnet link
	if strings.HasPrefix(*source, "magnet") {
		// getting metadata is fast enough that going to peers serially is fine
		var metadataBytes []byte
		for _, cli := range peerClients {
			metadataBytes, err = cli.GetMetadata(infoHash)
			if err != nil {
				fmt.Printf("error getting metadata from %s: %s\n", cli.conn.RemoteAddr(), err)
			}
			if err == nil {
				break
			}
		}

		if len(metadataBytes) == 0 {
			panic("failed getting metadata from clients")
		}

		torrent, err = FromMetadataBytes(metadataBytes)
		if err != nil {
			panic("parsing metadata bytes: " + err.Error())
		}
	}

	// Ready to start p2p downloads

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

	// make job queue size the number of pieces, otherwise writing to an unbuffered channel will
	// block until "someone" reads that value
	jobQueue := make(chan pieceJob, len(torrent.PieceHashes))
	results := make(chan pieceResult)

	// spin up clients concurrently
	for _, p := range peerClients {
		p := p
		// start "worker" goroutine, i.e. peer client ready to download pieces
		go func() {
			defer p.Close()
			for job := range jobQueue {
				pieceBuf, err := p.GetPiece(job.index, job.length, job.hash)
				if err != nil {
					// place job back on queue
					jobQueue <- job
					// if the client didn't have the piece, just continue along, don't close the
					// peer connection
					if errors.Is(err, ErrNotInBitfield) {
						continue
					}
					// otherwise stop listening to jobQueue, defer will cleanup client connection
					return
				}
				results <- pieceResult{
					Index:     job.index,
					FilePiece: pieceBuf,
				}
			}
		}()
	}

	// send jobs to download pieces to jobQueue channel
	for i, hash := range torrent.PieceHashes {
		// all pieces are the full size except for the last piece
		length := torrent.PieceLength
		if i == len(torrent.PieceHashes)-1 {
			length = torrent.TotalLength - torrent.PieceLength*(len(torrent.PieceHashes)-1)
		}
		jobQueue <- pieceJob{
			index:  i,
			length: length,
			hash:   hash,
		}
	}

	// merge results into a final buffer
	resultBuf := make([]byte, torrent.TotalLength)
	var pieces int
	for pieces < len(torrent.PieceHashes) {
		piece := <-results

		// copy to final buffer
		copy(resultBuf[piece.Index*torrent.PieceLength:], piece.FilePiece)
		pieces++
		fmt.Printf("%0.2f%% done\n", float64(pieces)/float64(len(torrent.PieceHashes))*100)
	}

	// close job queue which will close all peer connections
	close(jobQueue)

	// break resultBuf into separate files
	var usedBytes int
	for _, file := range torrent.Files {
		outPath := filepath.Join(*outDir, file.FullPath)

		fmt.Printf("writing to file %q\n", outPath)
		// ensure the directory exists
		baseDir := filepath.Dir(outPath)
		_, err := os.Stat(baseDir)
		if os.IsNotExist(err) {
			err := os.MkdirAll(baseDir, os.ModePerm)
			if err != nil {
				panic("making directory: " + err.Error())
			}
		}

		// check integrity if hashes were provided in metadata
		fileRaw := resultBuf[usedBytes : usedBytes+file.Length]
		if file.SHA1Hash != "" {
			hash := sha1.Sum(fileRaw)
			if !bytes.Equal(hash[:], []byte(file.SHA1Hash)) {
				panic(fmt.Sprintf("%q failed SHA-1 hash comparison", file.FullPath))
			}
		}
		if file.MD5Hash != "" {
			hash := md5.Sum(fileRaw)
			if !bytes.Equal(hash[:], []byte(file.MD5Hash)) {
				panic(fmt.Sprintf("%q failed MD5 hash comparison", file.FullPath))
			}
		}

		// write to the file
		err = os.WriteFile(outPath, fileRaw, os.ModePerm)
		usedBytes += file.Length
		if err != nil {
			panic("writing to file: " + err.Error())
		}
	}
}
