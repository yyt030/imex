package export

import (
	"context"
	"io"
	"sync"

	"github.com/pkg/errors"
)

// shardBlockSize matches the bufio size the DEL and SQL writers are built with,
// so a full buffer flush is handed over as exactly one block instead of being
// split and re-stitched.
const shardBlockSize = 256 * 1024

// errShardAborted reports that the output writer stopped consuming this shard,
// because writing the final file already failed. The run is failing anyway; the
// error exists so the producer stops reading rows instead of blocking forever on
// a channel nobody drains.
var errShardAborted = errors.New("shard output consumer stopped")

// shardPipe is the hand-off between one shard's writers and the single goroutine
// that owns the output file position.
//
// Parallel export used to write one temporary file per shard and concatenate them
// afterwards: every byte reached the disk twice, the run needed twice the output
// size in free space, and nothing could be written until the last shard finished.
// Streaming keeps the shards concurrent and serialises only the file position: a
// producer fills a block, hands it over, and waits if the writer has not reached
// its shard yet. In-flight memory is therefore blocks x blockSize per shard.
type shardPipe struct {
	ctx       context.Context
	ch        chan []byte
	abort     chan struct{}
	shutdown  sync.Once
	pending   []byte
	blockSize int
	closed    bool
}

func newShardPipe(ctx context.Context, blocks, blockSize int) *shardPipe {
	if blocks < 0 {
		blocks = 0
	}
	if blockSize <= 0 {
		blockSize = shardBlockSize
	}
	return &shardPipe{
		ctx:       ctx,
		ch:        make(chan []byte, blocks),
		abort:     make(chan struct{}),
		blockSize: blockSize,
	}
}

// shardStreamQueueBlocks sizes the look-ahead of every shard pipe.
//
// It deliberately does not reuse --pipeline-buffer: that option counts rows in
// the read-to-write pipeline, and a shallow byte queue turns the single output
// writer into a stopwatch — producers fill their queue, stall, and the writer
// then waits for the next shard instead of always having blocks ready to drain.
// The budget is fixed and split across the shards, so in-flight memory stays
// around budget regardless of the thread count.
//
// The depth is not a detail: on a 4-core box, interleaved 7-run A/B of a 500k-row
// SQL dump measured 1.09 s for the temporary-file design, 1.24~1.31 s for
// 2.5 MiB and 8 MiB of look-ahead per shard (14~20% slower, because the writer
// keeps waiting on producers that stalled), and 1.09 s again at 16 MiB — parity,
// while writing half as many bytes to disk.
func shardStreamQueueBlocks(shards int) int {
	const budget = 64 << 20 // bytes across all shards
	if shards < 1 {
		shards = 1
	}
	blocks := budget / shardBlockSize / shards
	if blocks < 2 {
		return 2
	}
	if blocks > 64 {
		return 64
	}
	return blocks
}

// Write implements io.Writer. Blocks are copies, not aliases of the caller's
// buffer: the bufio writer that calls us owns that memory and overwrites it on
// its next fill, while the consumer may still be writing the block to disk.
func (p *shardPipe) Write(b []byte) (int, error) {
	total := len(b)
	for len(b) > 0 {
		if p.pending == nil {
			p.pending = make([]byte, 0, p.blockSize)
		}
		space := p.blockSize - len(p.pending)
		if space <= 0 {
			if err := p.emit(); err != nil {
				return total - len(b), err
			}
			continue
		}
		n := len(b)
		if n > space {
			n = space
		}
		p.pending = append(p.pending, b[:n]...)
		b = b[n:]
		if len(p.pending) == p.blockSize {
			// Hand a full block over immediately rather than waiting for the next
			// Write: the consumer can start sooner and the producer never holds on
			// to a spare block.
			if err := p.emit(); err != nil {
				return total - len(b), err
			}
		}
	}
	return total, nil
}

func (p *shardPipe) emit() error {
	block := p.pending
	p.pending = nil
	select {
	case p.ch <- block:
		return nil
	case <-p.abort:
		return errShardAborted
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}

// Close hands the remainder to the consumer and marks end of stream. It is safe
// to call more than once and must be called even after a failed write, otherwise
// the consumer waits for an EOF that never arrives.
func (p *shardPipe) Close() error {
	if p.closed {
		return nil
	}
	var err error
	if len(p.pending) > 0 {
		err = p.emit()
	}
	p.closed = true
	close(p.ch)
	return err
}

// shutdown releases producers blocked on a pipe nobody reads any more, because
// writing the output already failed.
func (p *shardPipe) terminate() {
	p.shutdown.Do(func() { close(p.abort) })
}

var _ io.Writer = (*shardPipe)(nil)
