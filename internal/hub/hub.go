package hub

import (
	"context"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Hub aggregates state changes and emits Wails events on a 100ms ticker
// (REDESIGN §4.5). Slow-consumer snapshot events are coalesced, never queued.

type PostDelta struct {
	ID     int64  `json:"id"`
	Done   int    `json:"done"`
	Failed int    `json:"failed"`
	Total  int    `json:"total"`
	Status string `json:"status"`
}

type ImageDelta struct {
	PostRowID int64  `json:"postRowId"`
	ID        int64  `json:"id"`
	State     string `json:"state"`
}

type progress struct {
	Done      int64 `json:"done"`
	Total     int64 `json:"total"`
	PostRowID int64 `json:"postRowId"` // 进度所属的帖子行 id（前端用它跟随正在下载的帖子）
}

type Hub struct {
	mu       sync.Mutex
	dirty    map[int64]bool  // post row ids with pending changes
	imgDirty map[int64]bool  // image row ids
	imgPost  map[int64]int64 // image id -> post row id（进度事件要带上帖子 id）
	prog     map[int64]progress
	logLines []logLine
	stop     chan struct{}
}

type logLine struct {
	level, msg string
}

func New() *Hub {
	return &Hub{
		dirty:    map[int64]bool{},
		imgDirty: map[int64]bool{},
		imgPost:  map[int64]int64{},
		prog:     map[int64]progress{},
		stop:     make(chan struct{}),
	}
}

// Start begins the 100ms emission loop; ctx is the Wails runtime context.
func (h *Hub) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-t.C:
				h.emit(ctx)
			}
		}
	}()
}

func (h *Hub) Stop() { close(h.stop) }

func (h *Hub) MarkPostDirty(postRowID int64) {
	h.mu.Lock()
	h.dirty[postRowID] = true
	h.mu.Unlock()
}

func (h *Hub) MarkImageDirty(postRowID, imageID int64) {
	h.mu.Lock()
	h.dirty[postRowID] = true
	h.imgDirty[imageID] = true
	if len(h.imgPost) > 100000 {
		h.imgPost = map[int64]int64{} // 长跑进程避免无限增长
	}
	h.imgPost[imageID] = postRowID
	h.mu.Unlock()
}

// MarkProgress 记录单图下载字节；同 id 只保留最新值，随机头合并发出。
func (h *Hub) MarkProgress(imageID, done, total int64) {
	h.mu.Lock()
	h.prog[imageID] = progress{Done: done, Total: total, PostRowID: h.imgPost[imageID]}
	h.mu.Unlock()
}

func (h *Hub) Log(level, msg string) {
	h.mu.Lock()
	h.logLines = append(h.logLines, logLine{level, msg})
	h.mu.Unlock()
}

// Collect drains pending post ids (app.go re-reads from store).
func (h *Hub) CollectPosts() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]int64, 0, len(h.dirty))
	for id := range h.dirty {
		out = append(out, id)
	}
	h.dirty = map[int64]bool{}
	return out
}

func (h *Hub) CollectImages() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]int64, 0, len(h.imgDirty))
	for id := range h.imgDirty {
		out = append(out, id)
	}
	h.imgDirty = map[int64]bool{}
	return out
}

func (h *Hub) CollectLogs() []logLine {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.logLines
	h.logLines = nil
	return out
}

func (h *Hub) CollectProgress() map[int64]progress {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.prog) == 0 {
		return nil
	}
	out := h.prog
	h.prog = map[int64]progress{}
	return out
}

func (h *Hub) emit(ctx context.Context) {
	if ids := h.CollectPosts(); len(ids) > 0 {
		wruntime.EventsEmit(ctx, "posts:dirty", ids)
	}
	if ids := h.CollectImages(); len(ids) > 0 {
		wruntime.EventsEmit(ctx, "images:dirty", ids)
	}
	if prog := h.CollectProgress(); len(prog) > 0 {
		type P struct {
			ID        int64 `json:"id"`
			PostRowID int64 `json:"postRowId"`
			Done      int64 `json:"done"`
			Total     int64 `json:"total"`
		}
		out := make([]P, 0, len(prog))
		for id, p := range prog {
			out = append(out, P{ID: id, PostRowID: p.PostRowID, Done: p.Done, Total: p.Total})
		}
		wruntime.EventsEmit(ctx, "img:progress", out)
	}
	if lines := h.CollectLogs(); len(lines) > 0 {
		type L struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		out := make([]L, len(lines))
		for i, l := range lines {
			out[i] = L{l.level, l.msg}
		}
		wruntime.EventsEmit(ctx, "log:new", out)
	}
}
