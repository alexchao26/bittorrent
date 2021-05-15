# BitTorrent client in Go

Initially based off of a [blog post][jl-blog-post] from Jesse Li to implement the [OG BitTorrent protocol][BEP0003].

## Public Domain (aka Legal) Torrents for Testing
-  Ubuntu Server LTS via [torrent][ubuntu-torrent-url]
    - single file torrent
- [NASA Torrents][nasa-torrents], in particular these [images from the Mars Viking Orbiter][example-nasa-torrent].
    - multi-file torrent
    - magnet link as well

## Supports
- Original Spec ([BEP0003][])
    - Multi-file .torrent files
    - Original and Compact Peer List formats ([BEP0023][])

## TODO
- [ ] Implement 'endgame mode' as described in [BEP0003][] for downloading the last few pieces
- [ ] Seeding in OG .torrent protocol
- [ ] Magnet links - might go w/ udp trackers
    - [ ] UDP Trackers ([BEP0015][])
        - can acquire a list of peers (ip & port) from a UDP tracker url
    - [ ] UDP Extensions ([BEP0041][])
    - [ ] Extension to download metadata from peers ([BEP0009][])
- [ ] DHT
- [ ] PEX
- [ ] Announce list - i.e. Multitracker Metadata Extension ([BEP0012])
- [ ] Some pretty terminal visual of pieces being downloaded?

<!-- reference links -->
[jl-blog-post]: https://blog.jse.li/posts/torrent/
[ubuntu-torrent-url]: https://ubuntu.com/download/alternative-downloads
[BEP0003]: http://bittorrent.org/beps/bep_0003.html 'original bittorrent spec'
[BEP0015]: http://bittorrent.org/beps/bep_0015.html 'UDP Trackers'
[BEP0009]:  http://bittorrent.org/beps/bep_0009.html 'Extension for Peers to Send Metadata Files'
[BEP0041]: http://bittorrent.org/beps/bep_0041.html 'UDP Extensions'
[BEP0012]: http://bittorrent.org/beps/bep_0012.html 'Multitracker Metadata Extension'
[nasa-torrents]: https://academictorrents.com/collection/nasa-datasets 'Archives of NASA torrents'
[example-nasa-torrent]: https://academictorrents.com/details/059ed25558b4587143db637ac3ca94bebb57d88d
[BEP0023]: http://bittorrent.org/beps/bep_0023.html 'Compact Peer Lists'