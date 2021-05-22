package main

import (
	"flag"
	"syscall"

	"github.com/alexchao26/bittorrent"
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

	d, err := bittorrent.NewDownload(*source)
	if err != nil {
		panic("starting download: " + err.Error())
	}
	err = d.Run(*outDir)
	if err != nil {
		panic("runing download: " + err.Error())
	}
}
