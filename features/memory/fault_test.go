package memory_test

import (
	"context"
	"time"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/memory"
)

// faultStore is a memstore whose operations fail on demand, to check how the service reports storage errors.
// A nil error means the operation goes through to the real store.
type faultStore struct {
	*memstore.Store
	errGet, errGetPath, errCreate, errUpdate, errDelete, errUsers, errPurge, errEnabled error
	errGetVec, errPutVec                                                                error
	errList                                                                             map[string]error // by login
	// onEnabled, when set, is called by every MemoryEnabled lookup (a signal that a background job started).
	onEnabled func()
}

func (f *faultStore) MemoryEnabled(ctx context.Context, login string) (bool, error) {
	if f.onEnabled != nil {
		defer f.onEnabled()
	}
	if f.errEnabled != nil {
		return false, f.errEnabled
	}
	return f.Store.MemoryEnabled(ctx, login)
}

func (f *faultStore) ListMemoryEntries(ctx context.Context, login string, statuses ...string) ([]*store.MemoryEntry, error) {
	if err := f.errList[login]; err != nil {
		return nil, err
	}
	return f.Store.ListMemoryEntries(ctx, login, statuses...)
}

func (f *faultStore) GetMemoryEntry(ctx context.Context, login, id string) (*store.MemoryEntry, error) {
	if f.errGet != nil {
		return nil, f.errGet
	}
	return f.Store.GetMemoryEntry(ctx, login, id)
}

func (f *faultStore) GetMemoryEntryByPath(ctx context.Context, login, path string) (*store.MemoryEntry, error) {
	if f.errGetPath != nil {
		return nil, f.errGetPath
	}
	return f.Store.GetMemoryEntryByPath(ctx, login, path)
}

func (f *faultStore) CreateMemoryEntry(ctx context.Context, e *store.MemoryEntry, l store.MemoryLimits) (*store.MemoryEntry, error) {
	if f.errCreate != nil {
		return nil, f.errCreate
	}
	return f.Store.CreateMemoryEntry(ctx, e, l)
}

func (f *faultStore) UpdateMemoryEntry(ctx context.Context, e *store.MemoryEntry, l store.MemoryLimits) (*store.MemoryEntry, error) {
	if f.errUpdate != nil {
		return nil, f.errUpdate
	}
	return f.Store.UpdateMemoryEntry(ctx, e, l)
}

func (f *faultStore) DeleteMemoryEntry(ctx context.Context, login, id, expect string) error {
	if f.errDelete != nil {
		return f.errDelete
	}
	return f.Store.DeleteMemoryEntry(ctx, login, id, expect)
}

func (f *faultStore) ListMemoryUsers(ctx context.Context) ([]string, error) {
	if f.errUsers != nil {
		return nil, f.errUsers
	}
	return f.Store.ListMemoryUsers(ctx)
}

func (f *faultStore) PurgeDeletedMemoryEntries(ctx context.Context, before time.Time) (int, error) {
	if f.errPurge != nil {
		return 0, f.errPurge
	}
	return f.Store.PurgeDeletedMemoryEntries(ctx, before)
}

func (f *faultStore) GetMemoryEmbeddings(ctx context.Context, login string) (map[string]store.EntryEmbedding, error) {
	if f.errGetVec != nil {
		return nil, f.errGetVec
	}
	return f.Store.GetMemoryEmbeddings(ctx, login)
}

func (f *faultStore) PutMemoryEmbeddings(ctx context.Context, login string, items []store.EntryEmbedding) error {
	if f.errPutVec != nil {
		return f.errPutVec
	}
	return f.Store.PutMemoryEmbeddings(ctx, login, items)
}

// plainStore hides everything but the MemoryStore interface: no embedding storage.
type plainStore struct{ store.MemoryStore }

// newFaulty is newFixture over a faultStore.
func newFaulty(o memory.Options) (*faultStore, *memory.Service, memory.Writer) {
	st := &faultStore{Store: memstore.New()}
	tick := now.Add(-time.Hour)
	clock := func() time.Time { tick = tick.Add(time.Second); return tick }
	st.Store.Now, o.Now = clock, clock
	return st, memory.New(st, o), memory.Writer{Login: "alice", Source: store.MemorySourceProxy}
}

// seed writes an active entry straight into the store (no embedding job is started, unlike Service.Create).
func seed(st store.MemoryStore, login, id, path, content string) *store.MemoryEntry {
	e, err := st.CreateMemoryEntry(context.Background(), &store.MemoryEntry{ID: id, UserLogin: login, Path: path, Title: id, Content: content,
		Scope: "personal", Repository: "default", Source: store.MemorySourceProxy, Status: store.MemoryActive,
		ContentHash: "h-" + id + "-" + content, CreatedAt: now, UpdatedAt: now}, store.MemoryLimits{})
	if err != nil {
		panic(err)
	}
	return e
}
