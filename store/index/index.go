package index

import (
	"bufio"
	"fmt"
	"github.com/Terry-Mao/bfs/libs/encoding/binary"
	"github.com/Terry-Mao/bfs/libs/errors"
	myos "github.com/Terry-Mao/bfs/store/os"
	log "github.com/golang/glog"
	"io"
	"os"
	"sync"
	"time"
)

// Index for fast recovery super block needle cache in memory, index is async
// append the needle meta data.
//
// index file format:
//  ---------------
// | super   block |
//  ---------------
// |     needle    |		   ----------------
// |     needle    |          |  key (int64)   |
// |     needle    | ---->    |  offset (uint) |
// |     needle    |          |  size (int32)  |
// |     ......    |           ----------------
// |     ......    |             int bigendian
//
// field     | explanation
// --------------------------------------------------
// key       | needle key (photo id)
// offset    | needle offset in super block (aligned)
// size      | needle data size

const (
	// signal command
	finish = 0
	ready  = 1
	// index size
	keySize    = 8
	offsetSize = 4
	sizeSize   = 4
	// constant 16
	indexSize = keySize + offsetSize + sizeSize
	// index offset
	keyOffset    = 0
	offsetOffset = keyOffset + keySize
	sizeOffset   = offsetOffset + offsetSize
	// 100mb
	fallocSize = 100 * 1024 * 1024
)

// Indexer used for fast recovery super block needle cache.
type Indexer struct {
	wg     sync.WaitGroup
	f      *os.File
	bw     *bufio.Writer
	signal chan int
	ring   *Ring
	//
	File    string  `json:"file"`
	LastErr error   `json:"last_err"`
	Offset  int64   `json:"offset"`
	Options Options `json:"options"`
	// status
	syncOffset int64
	closed     bool
	write      int
}

// Index index data.
type Index struct {
	Key    int64
	Offset uint32
	Size   int32
}

// parse parse buffer into indexer.
func (i *Index) parse(buf []byte) {
	i.Key = binary.BigEndian.Int64(buf)
	i.Offset = binary.BigEndian.Uint32(buf[offsetOffset:])
	i.Size = binary.BigEndian.Int32(buf[sizeOffset:])
	return
}

func (i *Index) String() string {
	return fmt.Sprintf(`
-----------------------------
Key:            %d
Offset:         %d
Size:           %d
-----------------------------
`, i.Key, i.Offset, i.Size)
}

// NewIndexer new a indexer for async merge index data to disk.
func NewIndexer(file string, options Options) (i *Indexer, err error) {
	var stat os.FileInfo
	i = &Indexer{}
	i.File = file
	i.closed = false
	i.syncOffset = 0
	i.Options = options
	i.ring = NewRing(options.RingBuffer)
	if i.f, err = os.OpenFile(file, os.O_RDWR|os.O_CREATE|myos.O_NOATIME, 0664); err != nil {
		log.Errorf("os.OpenFile(\"%s\") error(%v)", file, err)
		return nil, err
	}
	if stat, err = i.f.Stat(); err != nil {
		log.Errorf("index: %s Stat() error(%v)", i.File, err)
		return nil, err
	}
	if stat.Size() == 0 {
		if err = myos.Fallocate(i.f.Fd(), myos.FALLOC_FL_KEEP_SIZE, 0, fallocSize); err != nil {
			log.Errorf("index: %s fallocate() error(err)", i.File, err)
			i.Close()
			return nil, err
		}
	}
	i.bw = bufio.NewWriterSize(i.f, options.BufferSize)
	i.wg.Add(1)
	i.signal = make(chan int, 1)
	go i.merge()
	return
}

// Signal signal the write job merge index data.
func (i *Indexer) Signal() {
	if i.closed {
		return
	}
	select {
	case i.signal <- ready:
	default:
	}
}

// Add append a index data to ring.
func (i *Indexer) Add(key int64, offset uint32, size int32) (err error) {
	var index *Index
	if i.LastErr != nil {
		return i.LastErr
	}
	if index, err = i.ring.Set(); err != nil {
		i.LastErr = err
		return
	}
	index.Key = key
	index.Offset = offset
	index.Size = size
	i.ring.SetAdv()
	if i.ring.Buffered() > i.Options.MergeAtWrite {
		i.Signal()
	}
	return
}

// Write append index needle to disk.
// WARN can't concurrency with merge and write.
// ONLY used in super block recovery!!!!!!!!!!!
func (i *Indexer) Write(key int64, offset uint32, size int32) (err error) {
	if i.LastErr != nil {
		return i.LastErr
	}
	if err = binary.BigEndian.WriteInt64(i.bw, key); err != nil {
		i.LastErr = err
		return
	}
	if err = binary.BigEndian.WriteUint32(i.bw, offset); err != nil {
		i.LastErr = err
		return
	}
	if err = binary.BigEndian.WriteInt32(i.bw, size); err == nil {
		i.Offset += indexSize
		i.write++
	} else {
		i.LastErr = err
	}
	return
}

// sync sync the in-memory data flush to disk.
func (i *Indexer) sync() (err error) {
	var (
		fd     uintptr
		offset int64
		size   int64
	)
	// append N times call flush then clean the os page cache
	// page cache no used here...
	// after upload a photo, we cache in our own cache server.
	offset = i.syncOffset
	size = i.Offset - i.syncOffset
	if i.write < i.Options.MergeAtWrite {
		return
	}
	i.write = 0
	fd = i.f.Fd()
	if i.Options.Syncfilerange {
		if err = myos.Syncfilerange(fd, offset, size, myos.SYNC_FILE_RANGE_WRITE); err != nil {
			i.LastErr = err
			log.Errorf("index: %s Syncfilerange() error(%v)", i.File, err)
			return
		}
	} else {
		if err = myos.Fdatasync(fd); err != nil {
			i.LastErr = err
			log.Errorf("index: %s Fdatasync() error(%v)", i.File, err)
			return
		}
	}
	if err = myos.Fadvise(fd, offset, size, myos.POSIX_FADV_DONTNEED); err == nil {
		i.syncOffset = i.Offset
	} else {
		log.Errorf("index: %s Fadvise() error(%v)", i.File, err)
		i.LastErr = err
	}
	return
}

// Flush flush writer buffer.
func (i *Indexer) Flush() (err error) {
	if i.LastErr != nil {
		return i.LastErr
	}
	if err = i.bw.Flush(); err != nil {
		i.LastErr = err
		log.Errorf("index: %s Flush() error(%v)", i.File, err)
		return
	}
	err = i.sync()
	return
}

// mergeRing get index data from ring then write to disk.
func (i *Indexer) mergeRing() (err error) {
	var index *Index
	for {
		if index, err = i.ring.Get(); err != nil {
			err = nil
			break
		}
		if err = i.Write(index.Key, index.Offset, index.Size); err != nil {
			log.Errorf("index: %s Write() error(%v)", i.File, err)
			break
		}
		i.ring.GetAdv()
	}
	return
}

// merge merge from ring index data, then write to disk.
func (i *Indexer) merge() {
	var (
		err error
		sig int
	)
	log.Infof("index: %s write job start", i.File)
	for {
		select {
		case sig = <-i.signal:
		case <-time.After(i.Options.MergeAtTime):
			sig = ready
		}
		if sig != ready {
			break
		}
		if err = i.mergeRing(); err != nil {
			break
		}
		if err = i.Flush(); err != nil {
			break
		}
	}
	i.mergeRing()
	i.write = 0
	i.Flush()
	i.wg.Done()
	log.Warningf("index: %s write job exit", i.File)
	return
}

// Scan scan a indexer file.
func (i *Indexer) Scan(r *os.File, fn func(*Index) error) (err error) {
	var (
		data []byte
		fi   os.FileInfo
		fd   = r.Fd()
		ix   = &Index{}
		rd   = bufio.NewReaderSize(r, i.Options.BufferSize)
	)
	log.Infof("scan index: %s", i.File)
	// advise sequential read
	if fi, err = r.Stat(); err != nil {
		log.Errorf("index: %s Stat() error(%v)", i.File)
		return
	}
	if err = myos.Fadvise(fd, 0, fi.Size(), myos.POSIX_FADV_SEQUENTIAL); err != nil {
		log.Errorf("index: %s Fadvise() error(%v)", i.File)
		return
	}
	if _, err = r.Seek(0, os.SEEK_SET); err != nil {
		log.Errorf("index: %s Seek() error(%v)", i.File, err)
		return
	}
	for {
		if data, err = rd.Peek(indexSize); err != nil {
			break
		}
		ix.parse(data)
		if _, err = rd.Discard(indexSize); err != nil {
			break
		}
		if log.V(1) {
			log.Info(ix.String())
		}
		if err = fn(ix); err != nil {
			break
		}
	}
	if err == io.EOF {
		// advise no need page cache
		if err = myos.Fadvise(fd, 0, fi.Size(), myos.POSIX_FADV_DONTNEED); err != nil {
			log.Errorf("index: %s Fadvise() error(%v)", i.File)
			return
		}
		err = nil
		log.Infof("scan index: %s [ok]", i.File)
	} else {
		log.Infof("scan index: %s error(%v) [failed]", i.File, err)
	}
	return
}

// Recovery recovery needle cache meta data in memory, index file  will stop
// at the right parse data offset.
func (i *Indexer) Recovery(fn func(*Index) error) (err error) {
	var offset int64
	if i.Scan(i.f, func(ix *Index) error {
		offset += int64(indexSize)
		return fn(ix)
	}); err != nil {
		return
	}
	// reset b.w offset, discard left space which can't parse to a needle
	if _, err = i.f.Seek(offset, os.SEEK_SET); err != nil {
		log.Errorf("index: %s Seek() error(%v)", i.File, err)
	}
	return
}

// Open open the closed indexer, must called after NewIndexer.
func (i *Indexer) Open() (err error) {
	if !i.closed {
		return
	}
	if i.f, err = os.OpenFile(i.File, os.O_RDWR|myos.O_NOATIME, 0664); err != nil {
		log.Errorf("os.OpenFile(\"%s\") error(%v)", i.File, err)
		return
	}
	i.bw.Reset(i.f)
	i.closed = false
	i.LastErr = nil
	i.wg.Add(1)
	go i.merge()
	return
}

// Close close the indexer file.
func (i *Indexer) Close() {
	var err error
	if i.signal != nil {
		i.signal <- finish
		i.wg.Wait()
	}
	if i.f != nil {
		if err = i.Flush(); err != nil {
			log.Errorf("index: %s Flush() error(%v)", i.File, err)
		}
		if err = i.f.Sync(); err != nil {
			log.Errorf("index: %s Sync() error(%v)", i.File, err)
		}
		if err = i.f.Close(); err != nil {
			log.Errorf("index: %s Close() error(%v)", i.File, err)
		}
	}
	i.closed = true
	i.LastErr = errors.ErrIndexClosed
	return
}

// Destroy destroy the indexer.
func (i *Indexer) Destroy() {
	if !i.closed {
		i.Close()
	}
	os.Remove(i.File)
}
