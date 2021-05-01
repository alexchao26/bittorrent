package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	filename := flag.String("file", "", "torrent file")
	outDir := flag.String("outdir", "./", "output directory")
	flag.Parse()
	if *filename == "" {
		panic("file flag is required")
	}

	tf, err := ParseTorrentFile(*filename)
	if err != nil {
		panic(err)
	}

	// peerID is generated randomly
	peerID := [20]byte{}
	rand.Read(peerID[:])
	// doesn't really matter for just leeching
	port := 6881

	trackerURL, err := tf.BuildTrackerURL(peerID, port)
	if err != nil {
		panic(err)
	}

	peers, err := GetPeersFromTracker(trackerURL)
	if err != nil {
		panic(err)
	}

	fmt.Println("Peers:", len(peers))

	if len(peers) == 0 {
		panic("no peers!")
	}

	// make job queue size the number of pieces, otherwise unbuffered channels
	// block until "someone" reads each written value off the channel
	jobQueue := make(chan *Job, len(tf.PieceHashes))
	results := make(chan *Piece)

	// spin up clients concurrently
	for _, peer := range peers {
		peer := peer
		go func() {
			peerCli, err := NewPeerClient(peer, tf.InfoHash, peerID)
			if err != nil {
				fmt.Printf("error creating client with %s: %v\n", peer.String(), err)
				return
			}

			// start "worker", i.e. client listening for jobs
			peerCli.ListenForJobs(jobQueue, results)
		}()
	}

	// send jobs to download pieces to jobQueue channel
	for i, hash := range tf.PieceHashes {
		// all pieces are the full size except for the last piece?
		length := tf.PieceLength
		if i == len(tf.PieceHashes)-1 {
			length = tf.Length - tf.PieceLength*(len(tf.PieceHashes)-1)
		}
		jobQueue <- &Job{
			Index:  i,
			Length: length,
			Hash:   hash,
		}
	}

	// merge results into a final buffer
	resultBuf := make([]byte, tf.Length)
	var pieces int
	for pieces < len(tf.PieceHashes) {
		piece := <-results

		// copy to final buffer
		copy(resultBuf[piece.Index*tf.PieceLength:], piece.FilePiece)
		pieces++
		fmt.Printf("%0.2f%% done\n", float64(pieces)/float64(len(tf.PieceHashes))*100)
	}

	// close job queue which will close all peer connections
	close(jobQueue)

	outPath := filepath.Join(*outDir, tf.Name)
	fmt.Printf("writing to file %q\n", outPath)
	err = os.WriteFile(outPath, resultBuf, os.ModePerm)
	if err != nil {
		panic("writing to file" + err.Error())
	}
}
