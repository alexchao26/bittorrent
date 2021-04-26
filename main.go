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
	// doesn't matter? maybe important for accepting requests from others?
	port := 6881

	trackerURL, err := tf.BuildTrackerURL(peerID, port)
	if err != nil {
		panic(err)
	}

	resp, err := GetPeersFromTracker(trackerURL)
	if err != nil {
		panic(err)
	}
	peers, err := UnmarshalPeers([]byte(resp.Peers))
	if err != nil {
		panic(err)
	}

	if len(peers) == 0 {
		panic("no peers!")
	}

	fmt.Println("interval", resp.Interval)
	fmt.Println("Peers:", len(peers))

	// make job queue size the number of pieces, otherwise unbuffered channels
	// block until "someone" reads a value off hte channel
	jobQueue := make(chan *Job, len(tf.PieceHashes))
	results := make(chan *PieceResult)

	// spin up clients concurrently
	for _, peer := range peers {
		peer := peer
		go func() {
			peerCli, err := NewPeerClient(peer, tf.InfoHash, peerID)
			if err != nil {
				fmt.Printf("creating client with %s: %v\n", peer.String(), err)
				return
			}

			// start worker
			peerCli.ListenForJobs(jobQueue, results)
		}()
	}

	// send jobs to download pieces to jobQueue channel
	for i, hash := range tf.PieceHashes {
		// all pieces are the full size except for the last piece?
		length := tf.PieceLength
		if i == len(tf.PieceHashes)-1 {
			length = tf.Length - tf.PieceLength*(len(tf.PieceHashes)-1)
			fmt.Printf("Total len %d, pieceLen %d\n", tf.Length, tf.PieceLength)
			fmt.Println("length is", length)
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

	fmt.Println("writing to file")
	err = os.WriteFile(filepath.Join(*outDir, tf.Name), resultBuf, os.ModePerm)
	if err != nil {
		panic("writing to file" + err.Error())
	}
}
