package minigen

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

var tileStreamMagic = [8]byte{'T', 'T', 'M', 'G', '0', '0', '0', '1'}

// TileStream is the generator's private, sequential interchange format. It
// deliberately avoids SQLite: tinySQL consumes it through ImportTiles.
type TileStream struct {
	path     string
	metadata map[string]string
}
type tileStreamWriter struct {
	file   *os.File
	writer *bufio.Writer
	closed bool
}

func createTileStream(path string, cfg Config, bounds Bounds) (*tileStreamWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("minigen: create tile stream: %w", err)
	}
	metadata, err := json.Marshal(tileStreamMetadata(cfg, bounds))
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if len(metadata) > 1<<20 {
		_ = f.Close()
		return nil, fmt.Errorf("minigen: tile stream metadata too large")
	}
	writer := bufio.NewWriterSize(f, 256<<10)
	if _, err := writer.Write(tileStreamMagic[:]); err != nil {
		_ = f.Close()
		return nil, err
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(metadata)))
	if _, err := writer.Write(size[:]); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := writer.Write(metadata); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &tileStreamWriter{file: f, writer: writer}, nil
}
func (w *tileStreamWriter) Write(z, x, y int, data []byte) error {
	if z < 0 || z > 30 || x < 0 || y < 0 || len(data) > 64<<20 {
		return fmt.Errorf("minigen: invalid generated tile")
	}
	var head [17]byte
	head[0] = byte(z)
	binary.BigEndian.PutUint32(head[1:5], uint32(x))
	binary.BigEndian.PutUint32(head[5:9], uint32(y))
	binary.BigEndian.PutUint64(head[9:17], uint64(len(data)))
	if _, err := w.writer.Write(head[:]); err != nil {
		return err
	}
	_, err := w.writer.Write(data)
	return err
}
func (w *tileStreamWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.writer.Flush(); err != nil {
		_ = w.file.Close()
		return err
	}
	return w.file.Close()
}

func OpenTileStream(path string) (*TileStream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var magic [8]byte
	if _, err = io.ReadFull(f, magic[:]); err != nil {
		return nil, err
	}
	if magic != tileStreamMagic {
		return nil, fmt.Errorf("minigen: invalid tile stream")
	}
	var n [4]byte
	if _, err = io.ReadFull(f, n[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size > 1<<20 {
		return nil, fmt.Errorf("minigen: invalid tile stream metadata")
	}
	data := make([]byte, size)
	if _, err = io.ReadFull(f, data); err != nil {
		return nil, err
	}
	metadata := map[string]string{}
	if err = json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}
	return &TileStream{path: path, metadata: metadata}, nil
}
func (s *TileStream) Metadata() map[string]string {
	out := make(map[string]string, len(s.metadata))
	for k, v := range s.metadata {
		out[k] = v
	}
	return out
}
func (s *TileStream) Scan(ctx context.Context, visit func(z, x, y int, data []byte) error) error {
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Seek(8, io.SeekStart); err != nil {
		return err
	}
	var n [4]byte
	if _, err = io.ReadFull(f, n[:]); err != nil {
		return err
	}
	if _, err = f.Seek(int64(binary.BigEndian.Uint32(n[:])), io.SeekCurrent); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var h [17]byte
		if _, err = io.ReadFull(f, h[:]); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		size := binary.BigEndian.Uint64(h[9:17])
		if size > 64<<20 {
			return fmt.Errorf("minigen: invalid tile stream data size")
		}
		data := make([]byte, size)
		if _, err = io.ReadFull(f, data); err != nil {
			return err
		}
		if err := visit(int(h[0]), int(binary.BigEndian.Uint32(h[1:5])), int(binary.BigEndian.Uint32(h[5:9])), data); err != nil {
			return err
		}
	}
}
