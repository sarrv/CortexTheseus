package torrentfs

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"github.com/anacrolix/missinggo/slices"
	"github.com/bradfitz/iter"
	"github.com/edsrzf/mmap-go"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/CortexFoundation/CortexTheseus/log"
	"github.com/CortexFoundation/CortexTheseus/params"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/mmap_span"
	"github.com/anacrolix/torrent/storage"

	"github.com/anacrolix/dht"
)

const (
	defaultBytesLimitation          = 512 * 1024
	queryTimeInterval               = 1
	removeTorrentChanBuffer         = 16
	newTorrentChanBuffer            = 32
	updateTorrentChanBuffer         = 32
	expansionFactor         float64 = 1.25
	// Pending for gotInfo
	torrentPending     = 0
	torrentPaused      = 1
	torrentRunning     = 2
	torrentSeeding     = 3
	defaultTmpFilePath = ".tmp"
)

// Torrent ...
type Torrent struct {
	*torrent.Torrent
	bytesRequested  int64
	bytesLimitation int64
	bytesCompleted  int64
	bytesMissing    int64
	status          int64
  torrentPath     string
}

func (t *Torrent) GetTorrent() {
	<-t.GotInfo()
	if t.status != torrentPending {
		return
	}

	log.Debug("Torrent gotInfo finished")

	f, _ := os.Create(t.torrentPath)
	log.Debug("Write torrent file", "path", t.torrentPath)
	if err := t.Metainfo().Write(f); err != nil {
		log.Error("Error while write torrent file", "error", err)
	}

	defer f.Close()
	t.status = torrentPaused
}

func (t *Torrent) Seed() {
	t.Torrent.VerifyData()
	t.Torrent.DownloadAll()
	t.status = torrentSeeding
}

func (t *Torrent) Seeding() bool {
	return t.status == torrentSeeding
}

// Pause ...
func (t *Torrent) Pause() {
	if t.status != torrentPaused {
		t.status = torrentPaused
		t.Torrent.Drop()
	}
}

// Paused ...
func (t *Torrent) Paused() bool {
	return t.status == torrentPaused
}

// Run ...
func (t *Torrent) Run() {
	if t.status == torrentRunning {
		return
	}
	t.Torrent.DownloadAll()
	t.status = torrentRunning
}

// Running ...
func (t *Torrent) Running() bool {
	return t.status == torrentRunning
}

// Pending ...
func (t *Torrent) Pending() bool {
	return t.status == torrentPending
}

// TorrentManager ...
type TorrentManager struct {
	client        *torrent.Client
	torrents      map[metainfo.Hash]*Torrent
	trackers      []string
	DataDir       string
	TmpDataDir    string
	closeAll      chan struct{}
	newTorrent    chan string
	removeTorrent chan string
	updateTorrent chan interface{}
	halt          bool
	mu            sync.Mutex
}

func (tm *TorrentManager) Close() error {
	close(tm.closeAll)
	log.Info("Torrent Download Manager Closed")
	return nil
}

func (tm *TorrentManager) NewTorrent(input string) error {
	tm.newTorrent <- input
	return nil
}

func (tm *TorrentManager) RemoveTorrent(input string) error {
	tm.removeTorrent <- input
	return nil
}

func (tm *TorrentManager) UpdateTorrent(input interface{}) error {
	tm.updateTorrent <- input
	return nil
}

func isMagnetURI(uri string) bool {
	return strings.HasPrefix(uri, "magnet:?xt=urn:btih:")
}

func (tm *TorrentManager) SetTrackers(trackers []string) {
	for _, tracker := range trackers {
		tm.trackers = append(tm.trackers, tracker)
	}
}

func mmapFile(name string) (mm mmap.MMap, err error) {
	f, err := os.Open(name)
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return
	}
	if fi.Size() == 0 {
		return
	}
	return mmap.MapRegion(f, -1, mmap.RDONLY, mmap.COPY, 0)
}

func verifyTorrent(info *metainfo.Info, root string) error {
	span := new(mmap_span.MMapSpan)
	for _, file := range info.UpvertedFiles() {
		filename := filepath.Join(append([]string{root, info.Name}, file.Path...)...)
		mm, err := mmapFile(filename)
		if err != nil {
			return err
		}
		if int64(len(mm)) != file.Length {
			return fmt.Errorf("file %q has wrong length, %d / %d", filename, int64(len(mm)), file.Length)
		}
		span.Append(mm)
	}
	for i := range iter.N(info.NumPieces()) {
		p := info.Piece(i)
		hash := sha1.New()
		_, err := io.Copy(hash, io.NewSectionReader(span, p.Offset(), p.Length()))
		if err != nil {
			return err
		}
		good := bytes.Equal(hash.Sum(nil), p.Hash().Bytes())
		if !good {
			return fmt.Errorf("hash mismatch at piece %d", i)
		}
	}
	return nil
}

func (tm *TorrentManager) AddTorrent(filePath string) {
	if _, err := os.Stat(filePath); err != nil {
		return
	}
	mi, err := metainfo.LoadFromFile(filePath)
	if err != nil {
		log.Error("Error while adding torrent", "Err", err)
		return
	}
	spec := torrent.TorrentSpecFromMetaInfo(mi)
	ih := spec.InfoHash
	log.Debug("Get torrent from local file", "InfoHash", ih.HexString())

	tm.mu.Lock()
	defer tm.mu.Unlock()
	if _, ok := tm.torrents[ih]; ok {
		log.Debug("Torrent was already existed. Skip", "InfoHash", ih.HexString())
		//tm.mu.Unlock()
		return
	}
	TmpDir := path.Join(tm.TmpDataDir, ih.HexString())
	ExistDir := path.Join(tm.DataDir, ih.HexString())

	useExistDir := false
	if _, err := os.Stat(ExistDir); err == nil {
		log.Debug("Seeding from existing file.", "InfoHash", ih.HexString())
		info, err := mi.UnmarshalInfo()
		if err != nil {
			log.Error("error unmarshalling info: ", "info", err)
		}
		if err := verifyTorrent(&info, ExistDir); err != nil {
			log.Warn("torrent failed verification:", "err", err)
		} else {
			useExistDir = true
		}
	}

	torrentPath := path.Join(tm.TmpDataDir, ih.HexString(), "torrent")
	
	if useExistDir {
		spec.Storage = storage.NewFile(ExistDir)

		if len(spec.Trackers) == 0 {
			spec.Trackers = append(spec.Trackers, []string{})
		}
		for _, tracker := range tm.trackers {
			spec.Trackers[0] = append(spec.Trackers[0], tracker)
		}
		t, _, _ := tm.client.AddTorrentSpec(spec)
		var ss []string
		slices.MakeInto(&ss, mi.Nodes)
		tm.client.AddDHTNodes(ss)
		tm.torrents[ih] = &Torrent{
			t,
			defaultBytesLimitation,
			int64(defaultBytesLimitation * expansionFactor),
			0,
			0,
			torrentPending,
			torrentPath,
		}
		//tm.mu.Unlock()
		tm.torrents[ih].Run()
	} else {
		spec.Storage = storage.NewFile(TmpDir)

		if len(spec.Trackers) == 0 {
			spec.Trackers = append(spec.Trackers, []string{})
		}
		for _, tracker := range tm.trackers {
			spec.Trackers[0] = append(spec.Trackers[0], tracker)
		}
		t, _, _ := tm.client.AddTorrentSpec(spec)
		var ss []string
		slices.MakeInto(&ss, mi.Nodes)
		tm.client.AddDHTNodes(ss)
		tm.torrents[ih] = &Torrent{
			t,
			defaultBytesLimitation,
			int64(defaultBytesLimitation * expansionFactor),
			0,
			0,
			torrentPending,
			torrentPath,
		}
		//tm.mu.Unlock()
		tm.torrents[ih].Run()
	}
}

func (tm *TorrentManager) AddMagnet(uri string) {
	spec, err := torrent.TorrentSpecFromMagnetURI(uri)
	if err != nil {
		log.Error("Error while adding magnet uri", "Err", err)
	}
	ih := spec.InfoHash
	dataPath := path.Join(tm.TmpDataDir, ih.HexString())
	torrentPath := path.Join(tm.TmpDataDir, ih.HexString(), "torrent")
	seedTorrentPath := path.Join(tm.DataDir, ih.HexString(), "torrent")
  log.Info("Torrent file path verify", "torrent", torrentPath, "seed torrent", seedTorrentPath)
	if _, err := os.Stat(seedTorrentPath); err == nil {
		tm.AddTorrent(seedTorrentPath)
		return
	} else if _, err := os.Stat(torrentPath); err == nil {
		tm.AddTorrent(torrentPath)
		return
	}
	log.Debug("Get torrent from magnet uri", "InfoHash", ih.HexString())

	tm.mu.Lock()
	defer tm.mu.Unlock()
	if _, ok := tm.torrents[ih]; ok {
		log.Warn("Torrent was already existed. Skip", "InfoHash", ih.HexString())
		//tm.mu.Unlock()
		return
	}

	spec.Storage = storage.NewFile(dataPath)
	if len(spec.Trackers) == 0 {
		spec.Trackers = append(spec.Trackers, []string{})
	}
	for _, tracker := range tm.trackers {
		spec.Trackers[0] = append(spec.Trackers[0], tracker)
	}
	t, _, err := tm.client.AddTorrentSpec(spec)
	tm.torrents[ih] = &Torrent{
		t,
		defaultBytesLimitation,
		int64(defaultBytesLimitation * expansionFactor),
		0,
		0,
		torrentPending,
		torrentPath,
	}
	//tm.mu.Unlock()
	log.Debug("Torrent is waiting for gotInfo", "InfoHash", ih.HexString())
  
	go tm.torrents[ih].GetTorrent()
}

// UpdateMagnet ...
func (tm *TorrentManager) UpdateMagnet(ih metainfo.Hash, BytesRequested int64) {
	log.Debug("Update torrent", "InfoHash", ih, "bytes", BytesRequested)
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if t, ok := tm.torrents[ih]; ok {
		t.bytesRequested = BytesRequested
		if t.bytesRequested > t.bytesLimitation {
			t.bytesLimitation = int64(float64(BytesRequested) * expansionFactor)
		}
	}
	//tm.mu.Unlock()
}

// DropMagnet ...
func (tm *TorrentManager) DropMagnet(uri string) bool {
	spec, err := torrent.TorrentSpecFromMagnetURI(uri)
	if err != nil {
		log.Warn("error while removing magnet", "error", err)
	}
	ih := spec.InfoHash
	if t, ok := tm.torrents[ih]; ok {
		t.Torrent.Drop()
		delete(tm.torrents, ih)
		return true
	}
	return false
}

// NewTorrentManager ...
func NewTorrentManager(config *Config) *TorrentManager {
	cfg := torrent.NewDefaultClientConfig()
	// (TODO) some network device may not support utp protocol, which results in burst of latency
	cfg.DisableUTP = false
	cfg.NoDHT = false
	cfg.DhtStartingNodes = dht.GlobalBootstrapAddrs
	cfg.DataDir = config.DataDir
	cfg.DisableEncryption = true
	cfg.ExtendedHandshakeClientVersion = params.VersionWithMeta
	listenAddr := &net.TCPAddr{}
	log.Info("Torrent client listening on", "addr", listenAddr)
	cfg.SetListenAddr(listenAddr.String())
	cfg.Seed = true
	//cfg.EstablishedConnsPerTorrent = 5
	//cfg.HalfOpenConnsPerTorrent = 3
	log.Info("Torrent client configuration", "config", cfg)
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		log.Error("Error while create torrent client", "err", err)
	}

	tmpFilePath := path.Join(config.DataDir, defaultTmpFilePath)
	if _, err := os.Stat(tmpFilePath); err == nil {
		os.Remove(tmpFilePath)
	}
	os.Mkdir(tmpFilePath, os.FileMode(os.ModePerm))

	TorrentManager := &TorrentManager{
		client:        cl,
		torrents:      make(map[metainfo.Hash]*Torrent),
		DataDir:       config.DataDir,
		TmpDataDir:    tmpFilePath,
		closeAll:      make(chan struct{}),
		newTorrent:    make(chan string, newTorrentChanBuffer),
		removeTorrent: make(chan string, removeTorrentChanBuffer),
		updateTorrent: make(chan interface{}, updateTorrentChanBuffer),
	}

	if len(config.DefaultTrackers) > 0 {
		//TorrentManager.SetTrackers(strings.Split(config.DefaultTrackers, ","))
		log.Info("Tracker list", "trackers", config.DefaultTrackers)
		TorrentManager.SetTrackers(config.DefaultTrackers)
		TorrentManager.SetTrackers(params.MainnetTrackers)
	}
	log.Info("Torrent client initialized")

	return TorrentManager
}

func (tm *TorrentManager) Start() error {

	go tm.mainLoop()
	go tm.listenTorrentProgress()

	return nil
}

func (tm *TorrentManager) mainLoop() {
	for {
		select {
		case torrent := <-tm.newTorrent:
			log.Debug("TorrentManager", "newTorrent", torrent)
			if isMagnetURI(torrent) {
				go tm.AddMagnet(torrent)
			} else {
				go tm.AddTorrent(torrent)
			}
		case torrent := <-tm.removeTorrent:
			if isMagnetURI(torrent) {
				go tm.DropMagnet(torrent)
			} else {
			}
		case msg := <-tm.updateTorrent:
			meta := msg.(FlowControlMeta)
			go tm.UpdateMagnet(meta.InfoHash, int64(meta.BytesRequested))
		case <-tm.closeAll:
			tm.halt = true
			tm.client.Close()
			return
		}
	}
}

const (
	loops = 10
)

func (tm *TorrentManager) listenTorrentProgress() {
	var counter uint64
	for counter = 0; ; counter++ {
		if tm.halt {
			return
		}
		var seeding_n int = 0
		var pending_n int = 0
		var progress_n int = 0
		for ih, t := range tm.torrents {
			if t.Seeding() {
				t.bytesCompleted = t.BytesCompleted()
				t.bytesMissing = t.BytesMissing()
				if counter >= loops {
					log.Debug("Torrent seeding",
						"InfoHash", ih.HexString(),
						"completed", t.bytesCompleted,
						"total", t.bytesCompleted+t.bytesMissing,
						"seeding", t.Torrent.Seeding(),
					)
					seeding_n += 1
				}
			} else if !t.Pending() {
				t.bytesCompleted = t.BytesCompleted()
				t.bytesMissing = t.BytesMissing()
				if t.bytesMissing == 0 {
					os.Symlink(
						path.Join(defaultTmpFilePath, ih.HexString()),
						path.Join(tm.DataDir, ih.HexString()),
					)
					t.Seed()
				} else if t.bytesCompleted >= t.bytesLimitation {
					t.Pause()
				} else if t.bytesCompleted < t.bytesLimitation {
					t.Run()
				}
				if counter >= loops {
					log.Debug("Torrent progress",
						"InfoHash", ih.HexString(),
						"completed", t.bytesCompleted,
						"requested", t.bytesLimitation,
						"total", t.bytesCompleted+t.bytesMissing,
						"status", t.status)
					progress_n += 1
				}
			} else {
				go t.GetTorrent()
				if counter >= loops {
					log.Debug("Torrent pending",
						"InfoHash", ih.HexString(),
						"completed", t.bytesCompleted,
						"requested", t.bytesLimitation,
						"total", t.bytesCompleted+t.bytesMissing,
						"status", t.status)
					pending_n += 1
				}
			}
		}
		if counter >= loops {
			log.Info("Torrent tasks working status", "progress", progress_n, "pending", pending_n, "seeding", seeding_n)
			counter = 0
		}
		time.Sleep(time.Second * queryTimeInterval)
	}
}
