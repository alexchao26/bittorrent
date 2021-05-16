package main

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
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

	// Two things we need to start the download
	var torrent TorrentFile
	var peerClients []*PeerClient
	var mut sync.Mutex

	// peerID is generated randomly
	peerID := [20]byte{}
	rand.Read(peerID[:])
	// doesn't really matter for just leeching
	port := 6881

	if strings.HasPrefix(*source, "magnet") {
		magnetLink, err := ParseMagnetLink(*source)
		if err != nil {
			panic("error parsing magnet link: " + err.Error())
		}

		fmt.Println("Parsed Magnet Link", magnetLink)

		var peerAddrs []net.TCPAddr
		var wg sync.WaitGroup
		wg.Add(len(magnetLink.TrackerURLs))
		for _, trackerURL := range magnetLink.TrackerURLs {
			trackerURL := trackerURL
			go func() {
				defer wg.Done()
				addrs, err := GetPeersFromTracker(trackerURL, magnetLink.InfoHash, peerID, port)
				if err != nil {
					fmt.Printf("error from tracker %q: %s\n", trackerURL, err.Error())
					return
				}
				mut.Lock()
				fmt.Println("\t\tpeers from ", trackerURL, ": ", len(addrs))
				peerAddrs = append(peerAddrs, addrs...)
				mut.Unlock()
			}()
		}
		wg.Wait()

		peerAddrs = DedupeAddrs(peerAddrs)
		fmt.Println("deduped peer count:", len(peerAddrs))

		var metadataBytes []byte
		wg.Add(len(peerAddrs))
		for _, addr := range peerAddrs {
			addr := addr
			go func() {
				defer wg.Done()
				cli, err := NewPeerClient(addr, magnetLink.InfoHash, peerID)
				if err != nil {
					fmt.Println("error creating client to", addr, err)
					return
				}
				mut.Lock()
				peerClients = append(peerClients, cli)
				mut.Unlock()
			}()
		}
		wg.Wait()

		fmt.Println("peer clients len", len(peerClients))

		for _, cli := range peerClients {
			fmt.Println("start getting from", cli.conn.RemoteAddr())
			metadataBytes, err = cli.GetMetadata(magnetLink.InfoHash)
			if err != nil {
				fmt.Println("ERROR cli.GetMetadata() ", err)
			}
			if err == nil {
				break
			}
		}

		fmt.Println("metadata bytes len", len(metadataBytes))

		if len(metadataBytes) == 0 {
			panic("failed getting metadata from clients")
		}

		torrent, err = FromMetadataBytes(metadataBytes)
		if err != nil {
			panic("parsing metadata bytes: " + err.Error())
		}
	} else if strings.HasSuffix(*source, ".torrent") {
		var err error
		torrent, err = ParseTorrentFile(*source)
		if err != nil {
			panic(err)
		}

		peerAddrs, err := GetPeersFromTracker(torrent.Announce, torrent.InfoHash, peerID, port)
		if err != nil {
			panic(err)
		}
		fmt.Println("Peers:", len(peerAddrs))

		if len(peerAddrs) == 0 {
			panic("no peers!")
		}

		var wg sync.WaitGroup
		for _, ip := range peerAddrs {
			// spin up peer clients concurrently
			ip := ip
			wg.Add(1)
			go func() {
				defer wg.Done()
				client, err := NewPeerClient(ip, torrent.InfoHash, peerID)
				if err != nil {
					fmt.Println("error connecting to", ip, err)
					return
				}
				mut.Lock()
				peerClients = append(peerClients, client)
				mut.Unlock()
			}()
		}
		wg.Wait()
	}

	// make job queue size the number of pieces, otherwise unbuffered channels
	// block until "someone" reads each written value off the channel
	jobQueue := make(chan *Job, len(torrent.PieceHashes))
	results := make(chan *Piece)

	// spin up clients concurrently
	for _, p := range peerClients {
		p := p
		go func() {
			// start "worker", i.e. client listening for jobs
			p.ListenForJobs(jobQueue, results)
		}()
	}

	// send jobs to download pieces to jobQueue channel
	for i, hash := range torrent.PieceHashes {
		// all pieces are the full size except for the last piece
		length := torrent.PieceLength
		if i == len(torrent.PieceHashes)-1 {
			length = torrent.TotalLength - torrent.PieceLength*(len(torrent.PieceHashes)-1)
		}
		jobQueue <- &Job{
			Index:  i,
			Length: length,
			Hash:   hash,
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
