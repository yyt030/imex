package export

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imex/internal/config"
)

// collect drains a pipe into memory until EOF, as writeShardStream would.
func collect(t *testing.T, p *shardPipe) [][]byte {
	t.Helper()

	var blocks [][]byte
	for block := range p.ch {
		blocks = append(blocks, block)
	}
	return blocks
}

// A producer writing arbitrary chunk sizes must hand over the same byte stream,
// with nothing duplicated or dropped at the block boundaries.
func TestShardPipe_ReassemblesWritesExactly(t *testing.T) {
	want := []byte(strings.Repeat("0123456789abcdef", 500))

	for _, blockSize := range []int{1, 2, 7, 4096, 1 << 20} {
		for _, writeSize := range []int{1, 3, blockSize, blockSize + 1, 5000} {
			p := newShardPipe(context.Background(), 4, blockSize)
			done := make(chan [][]byte, 1)
			go func() { done <- collect(t, p) }()

			var produced int64
			for produced < int64(len(want)) {
				end := int(produced) + writeSize
				if int64(end) > int64(len(want)) {
					end = len(want)
				}
				n, err := p.Write(want[produced:end])
				if err != nil {
					t.Fatalf("blockSize=%d writeSize=%d: Write: %v", blockSize, writeSize, err)
				}
				if n != end-int(produced) {
					t.Fatalf("blockSize=%d writeSize=%d: Write n=%d, want %d", blockSize, writeSize, n, end-int(produced))
				}
				produced = int64(end)
			}
			if err := p.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			var out bytes.Buffer
			for _, blk := range <-done {
				out.Write(blk)
			}
			if !bytes.Equal(out.Bytes(), want) {
				t.Fatalf("blockSize=%d writeSize=%d: stream differs (got %d bytes, want %d)",
					blockSize, writeSize, out.Len(), len(want))
			}
		}
	}
}

// No block handed to the consumer may exceed the configured size, otherwise the
// in-flight memory bound stops being predictable.
func TestShardPipe_BlocksNeverExceedBlockSize(t *testing.T) {
	const blockSize = 64
	p := newShardPipe(context.Background(), 8, blockSize)

	got := make(chan [][]byte, 1)
	go func() { got <- collect(t, p) }()

	payload := bytes.Repeat([]byte("x"), blockSize*5+10)
	if _, err := p.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, b := range <-got {
		if len(b) > blockSize {
			t.Fatalf("block of %d bytes exceeds blockSize %d", len(b), blockSize)
		}
	}
}

// The queue depth is the only look-ahead a shard gets: with the queue full and no
// consumer, the next write must block instead of growing without bound.
func TestShardPipe_BlocksWhenQueueIsFull(t *testing.T) {
	const blockSize = 16
	p := newShardPipe(context.Background(), 1, blockSize)

	blocked := make(chan error, 1)
	go func() {
		// One block fills the queue, the next has to wait for the consumer.
		_, err := p.Write(bytes.Repeat([]byte("y"), blockSize*2))
		if cerr := p.Close(); err == nil {
			err = cerr
		}
		blocked <- err
	}()

	select {
	case err := <-blocked:
		t.Fatalf("Write returned with a full queue and no consumer: %v", err)
	case <-time.After(150 * time.Millisecond):
		// expected: still blocked on the hand-off
	}

	// Releasing the consumer lets the producer finish.
	for range p.ch {
	}
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("Write after drain: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("producer did not resume after its queue drained")
	}
}

// If the output writer fails, it stops draining. Producers must notice and give
// up rather than block forever on a channel nobody reads.
func TestShardPipe_TerminateReleasesBlockedProducer(t *testing.T) {
	p := newShardPipe(context.Background(), 0, 8)

	errCh := make(chan error, 1)
	go func() {
		_, err := p.Write(bytes.Repeat([]byte("z"), 8))
		errCh <- err
	}()

	select {
	case <-errCh:
		t.Fatal("Write went through an unbuffered pipe with no consumer")
	case <-time.After(100 * time.Millisecond):
	}

	p.terminate()

	select {
	case err := <-errCh:
		if err != errShardAborted {
			t.Fatalf("Write error = %v, want %v", err, errShardAborted)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminate did not release the blocked producer")
	}
}

func TestShardPipe_ContextCancelReleasesProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := newShardPipe(ctx, 0, 8)

	errCh := make(chan error, 1)
	go func() {
		_, err := p.Write([]byte("12345678"))
		errCh <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Write error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation did not release the blocked producer")
	}
}

// The consumer waits for EOF, so every exit path has to close the pipe. Close is
// the deferred call in exportShard, hence it must tolerate repeats and still emit
// whatever is pending.
func TestShardPipe_CloseFlushesPendingAndIsRepeatable(t *testing.T) {
	p := newShardPipe(context.Background(), 4, 1024)
	if _, err := p.Write([]byte("tail")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := make(chan [][]byte, 1)
	go func() { got <- collect(t, p) }()

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	blocks := <-got
	if len(blocks) != 1 || string(blocks[0]) != "tail" {
		t.Fatalf("blocks = %q, want one block \"tail\"", blocks)
	}
}

// Real writers (bufio on top of the pipe) must be able to use it as an io.Writer
// end to end; this is the shape exportShard relies on.
func TestShardPipe_WorksWithBufferedWriter(t *testing.T) {
	p := newShardPipe(context.Background(), 2, 128)

	want := strings.Repeat("row\n", 1000)
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		for block := range p.ch {
			sb.Write(block)
		}
		done <- sb.String()
	}()

	bw := bufio.NewWriterSize(p, 4096)
	if _, err := io.WriteString(bw, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := <-done; got != want {
		t.Fatalf("stream length = %d, want %d", len(got), len(want))
	}
}

// Shards finish in whatever order the database serves them. The output must still
// be shard 0, shard 1, ... because that is what a single-threaded dump produces
// for the same table.
func TestShardStream_OutputOrderIsShardOrderNotFinishOrder(t *testing.T) {
	const shards = 4
	ctx := context.Background()
	pipes := make([]*shardPipe, shards)
	for i := range pipes {
		pipes[i] = newShardPipe(ctx, 1, 32)
	}

	out := t.TempDir() + "/streamed.del"
	e := streamTestExporter(t, "del", true, false, true)
	e.config.Output = out

	// Producer i writes its own shard marker, but shard i+1 starts earlier and
	// finishes first, so a finish-ordered merge would interleave.
	var producers sync.WaitGroup
	for i := len(pipes) - 1; i >= 0; i-- {
		producers.Add(1)
		idx := i
		go func() {
			defer producers.Done()
			defer func() { _ = pipes[idx].Close() }()
			body := strings.Repeat(fmt.Sprintf("S%d\n", idx), 200)
			if _, err := pipes[idx].Write([]byte(body)); err != nil {
				t.Errorf("shard %d write: %v", idx, err)
			}
		}()
	}

	if err := e.writeShardStream(pipes, []string{"id"}); err != nil {
		t.Fatalf("writeShardStream: %v", err)
	}
	producers.Wait()

	got := readOut(t, out)
	var want strings.Builder
	for i := 0; i < shards; i++ {
		want.WriteString(strings.Repeat(fmt.Sprintf("S%d\n", i), 200))
	}
	if got != want.String() {
		t.Fatalf("output is not in shard order:\n got prefix %q\nwant prefix %q", got[:min(len(got), 40)], want.String()[:40])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// The look-ahead is a fixed byte budget shared by the shards: enough slack that
// the single output writer always has blocks to drain, and small enough that a
// high --threads value does not multiply it.
func TestShardStreamQueueBlocks(t *testing.T) {
	cases := []struct {
		shards int
		want   int
	}{
		{1, 64},   // capped: one shard does not need 64 MiB of look-ahead
		{2, 64},   // capped at 16 MiB per shard
		{4, 64},   // exactly the budget: 16 MiB per shard
		{8, 32},   // 8 MiB per shard
		{16, 16},  // 4 MiB per shard
		{64, 4},   // 1 MiB per shard
		{256, 2},  // floored: a shard can still make progress while draining
		{1024, 2}, // floored
		{0, 64},   // treated as one shard
	}
	for _, c := range cases {
		if got := shardStreamQueueBlocks(c.shards); got != c.want {
			t.Errorf("shardStreamQueueBlocks(%d) = %d, want %d", c.shards, got, c.want)
		}
	}

	// Worst case memory is bounded even with the maximum thread count.
	if maxBytes := shardStreamQueueBlocks(256) * shardBlockSize * 256; maxBytes > 256<<20 {
		t.Errorf("256 shards hold %d bytes in flight, want <= %d", maxBytes, 256<<20)
	}
}

// TestParallelExportPipelineOptionNotice guards the record the parallel path
// writes when the hand-off options were tuned but cannot take effect. Silence is
// half the contract: the common run (both options at their defaults) must not
// gain a record, and neither may the single-threaded path, which reads them.
func TestParallelExportPipelineOptionNotice(t *testing.T) {
	untouched := config.NewExportConfig()
	if (&Exporter{config: untouched}).pipelineOptionsIgnoredByParallel() {
		t.Error("a run that left --batch-size and --pipeline-buffer at their defaults must not be told they are ignored")
	}

	widened := config.NewExportConfig()
	widened.BatchSize = 5000
	if !(&Exporter{config: widened}).pipelineOptionsIgnoredByParallel() {
		t.Error("--batch-size moved off its default is exactly what the notice exists for")
	}

	// 0 is a legal value meaning synchronous hand-off, and moving it is deliberate.
	synced := config.NewExportConfig()
	synced.PipelineBuffer = 0
	if !(&Exporter{config: synced}).pipelineOptionsIgnoredByParallel() {
		t.Error("--pipeline-buffer 0 is a deliberate move and has to be reported too")
	}
}
