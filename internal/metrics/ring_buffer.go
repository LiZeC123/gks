package metrics

import "sync"

// RingBuffer 是一个固定容量的并发安全环形缓冲区。
// 写满后，新数据会自然覆盖最旧的数据。
type RingBuffer struct {
	mu   sync.Mutex
	buf  []string
	next int // 下一个写入位置；写满后它也是当前最旧数据的位置
}

// NewRingBuffer 创建一个容量为 n 的 RingBuffer。
func NewRingBuffer(n int) *RingBuffer {
	if n <= 0 {
		panic("ringbuffer: n must be positive")
	}

	return &RingBuffer{
		buf: make([]string, n),
	}
}

// Write 并发安全地写入一个字符串。
// 当缓冲区满时，会覆盖最旧的数据。
func (r *RingBuffer) Write(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.buf[r.next] = s
	r.next++
	if r.next == len(r.buf) {
		r.next = 0
	}
}

// Snapshot 返回当前缓冲区内容的副本，按“从旧到新”排列。
// 返回的切片长度始终为容量 n。
// 未写满时，前面会包含初始零值字符串 ""。
func (r *RingBuffer) Snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := len(r.buf)
	out := make([]string, n)

	// next 指向下一个写入位置：
	// - 未满时，它是下一个空位；
	// - 已满时，它指向最旧数据。
	//
	// 因此从 next 开始复制，得到“从旧到新”的顺序。
	copy(out, r.buf[r.next:])
	copy(out[n-r.next:], r.buf[:r.next])

	return out
}
