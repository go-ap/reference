// +build storage_badger storage_all !storage_pgx,!storage_boltdb,!storage_fs,!storage_sqlite

package badger

import (
	"bytes"
	"encoding/json"
	"github.com/dgraph-io/badger/v3"
	pub "github.com/go-ap/activitypub"
	"github.com/go-ap/errors"
	ap "github.com/go-ap/fedbox/activitypub"
	"github.com/go-ap/fedbox/internal/cache"
	"github.com/go-ap/fedbox/storage"
	"github.com/go-ap/handlers"
	"github.com/go-ap/jsonld"
	s "github.com/go-ap/storage"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
	"os"
	"path"
	"time"
)

const (
	pathActors     = ap.ActorsType
	pathActivities = ap.ActivitiesType
	pathObjects    = ap.ObjectsType
)

type repo struct {
	d       *badger.DB
	baseURL string
	path    string
	cache   cache.CanStore
	logFn   loggerFn
	errFn   loggerFn
}

type loggerFn func(logrus.Fields, string, ...interface{})

// Config
type Config struct {
	Path    string
	BaseURL string
	LogFn   loggerFn
	ErrFn   loggerFn
}

var emptyLogFn = func(logrus.Fields, string, ...interface{}) {}

// New returns a new repo repository
func New(c Config) (*repo, error) {
	var err error
	c.Path, err = Path(c)
	if err != nil {
		return nil, err
	}
	b := repo{
		path:    c.Path,
		baseURL: c.BaseURL,
		cache:   cache.New(true),
		logFn:   emptyLogFn,
		errFn:   emptyLogFn,
	}
	if c.ErrFn != nil {
		b.errFn = c.ErrFn
	}
	if c.LogFn != nil {
		b.logFn = c.LogFn
	}
	return &b, nil
}

// Open opens the badger database if possible.
func (r *repo) Open() error {
	var (
		err error
		c badger.Options
	)
	c = badger.DefaultOptions(r.path).WithLogger(logger{ logFn: r.logFn, errFn: r.errFn })
	if r.path == "" {
		c.InMemory = true
	}
	r.d, err = badger.Open(c)
	if err != nil {
		err = errors.Annotatef(err, "unable to open storage")
	}
	return err
}

// Close closes the badger database if possible.
func (r *repo) Close() error {
	if r.d == nil {
		return nil
	}
	return r.d.Close()
}

// Load
func (r *repo) Load(i pub.IRI) (pub.Item, error) {
	var err error
	if r.Open(); err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := ap.FiltersFromIRI(i)
	if err != nil {
		return nil, err
	}

	it, _, err := r.loadFromPath(f)
	return it, err
}

func (r *repo) Create(col pub.CollectionInterface) (pub.CollectionInterface, error) {
	var err error
	err = r.Open()
	if err != nil {
		return col, err
	}
	defer r.Close()

	err = r.d.Update(func(tx *badger.Txn) error {
		_, err := createCollectionInPath(tx, col.GetLink())
		return err
	})
	return col, err
}

// Save
func (r *repo) Save(it pub.Item) (pub.Item, error) {
	var err error
	err = r.Open()
	if err != nil {
		return it, err
	}
	defer r.Close()

	if it, err = save(r, it); err == nil {
		op := "Updated"
		id := it.GetID()
		if !id.IsValid() {
			op = "Added new"
		}
		r.logFn(nil, "%s %s: %s", op, it.GetType(), it.GetLink())
	}

	return it, err
}

// IsLocalIRI shows if the received IRI belongs to the current instance
func (r repo) IsLocalIRI(i pub.IRI) bool {
	return i.Contains(pub.IRI(r.baseURL), false)
}

func onCollection(r *repo, col pub.IRI, it pub.Item, fn func(iris pub.IRIs) (pub.IRIs, error)) error {
	if pub.IsNil(it) {
		return errors.Newf("Unable to operate on nil element")
	}
	if len(col) == 0 {
		return errors.Newf("Unable to find collection")
	}
	if len(it.GetLink()) == 0 {
		return errors.Newf("Invalid collection, it does not have a valid IRI")
	}
	if !r.IsLocalIRI(col) {
		return errors.Newf("Unable to save to non local collection %s", col)
	}
	p := itemPath(col)

	err := r.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	return r.d.Update(func(tx *badger.Txn) error {
		iris := make(pub.IRIs, 0)

		rawKey := getObjectKey(p)
		if i, err := tx.Get(rawKey); err == nil {
			err = i.Value(func(raw []byte) error {
				err := jsonld.Unmarshal(raw, &iris)
				if err != nil {
					return errors.Annotatef(err, "Unable to unmarshal collection %s", p)
				}
				return nil
			})
		}
		var err error
		iris, err = fn(iris)
		if err != nil {
			return errors.Annotatef(err, "Unable operate on collection %s", p)
		}
		var raw []byte
		raw, err = jsonld.Marshal(iris)
		if err != nil {
			return errors.Newf("Unable to marshal entries in collection %s", p)
		}
		err = tx.Set(rawKey, raw)
		if err != nil {
			return errors.Annotatef(err, "Unable to save entries to collection %s", p)
		}
		return err
	})
}

// RemoveFrom
func (r *repo) RemoveFrom(col pub.IRI, it pub.Item) error {
	return onCollection(r, col, it, func(iris pub.IRIs) (pub.IRIs, error) {
		for k, iri := range iris {
			if iri.GetLink().Equals(it.GetLink(), false) {
				iris = append(iris[:k], iris[k+1:]...)
				break
			}
		}
		return iris, nil
	})
}

func addCollectionOnObject(r *repo, col pub.IRI) error {
	allStorageCollections := append(handlers.ActivityPubCollections, ap.FedboxCollections...)
	if ob, t := allStorageCollections.Split(col); handlers.ValidCollection(t) {
		// Create the collection on the object, if it doesn't exist
		if i, _ := r.LoadOne(ob); i != nil {
			if _, ok := t.AddTo(i); ok {
				_, err := r.Save(i)
				return err
			}
		}
	}
	return nil
}

// AddTo
func (r *repo) AddTo(col pub.IRI, it pub.Item) error {
	addCollectionOnObject(r, col)
	return onCollection(r, col, it, func(iris pub.IRIs) (pub.IRIs, error) {
		if iris.Contains(it.GetLink()) {
			return iris, nil
		}
		return append(iris, it.GetLink()), nil
	})
}

// Delete
func (r *repo) Delete(it pub.Item) (pub.Item, error) {
	var err error
	err = r.Open()
	if err != nil {
		return it, err
	}
	defer r.Close()
	var bucket handlers.CollectionType
	if pub.ActivityTypes.Contains(it.GetType()) {
		bucket = pathActivities
	} else if pub.ActorTypes.Contains(it.GetType()) {
		bucket = pathActors
	} else {
		bucket = pathObjects
	}
	if it, err = delete(r, it); err == nil {
		r.logFn(nil, "Added new %s: %s", bucket[:len(bucket)-1], it.GetLink())
	}
	return it, err
}

func getMetadataKey(p []byte) []byte {
	return bytes.Join([][]byte{p, []byte(metaDataKey)}, sep)
}

// PasswordSet
func (r *repo) PasswordSet(it pub.Item, pw []byte) error {
	path := itemPath(it.GetLink())
	err := r.Open()
	if err != nil {
		return err
	}
	defer r.Close()

	err = r.d.Update(func(tx *badger.Txn) error {
		pw, err = bcrypt.GenerateFromPassword(pw, -1)
		if err != nil {
			return errors.Annotatef(err, "Could not encrypt the pw")
		}
		m := storage.Metadata{
			Pw: pw,
		}
		entryBytes, err := jsonld.Marshal(m)
		if err != nil {
			return errors.Annotatef(err, "Could not marshal metadata")
		}
		err = tx.Set(getMetadataKey(path), entryBytes)
		if err != nil {
			return errors.Annotatef(err, "Could not insert entry: %s", path)
		}
		return nil
	})

	return err
}

// PasswordCheck
func (r *repo) PasswordCheck(it pub.Item, pw []byte) error {
	path := itemPath(it.GetLink())
	err := r.Open()
	if err != nil {
		return err
	}
	defer r.Close()

	m := storage.Metadata{}
	err = r.d.View(func(tx *badger.Txn) error {
		i, err := tx.Get(getMetadataKey(path))
		if err != nil {
			return errors.Annotatef(err, "Could not find metadata in path %s", path)
		}
		i.Value(func(raw []byte) error {
			err := jsonld.Unmarshal(raw, &m)
			if err != nil {
				return errors.Annotatef(err, "Could not unmarshal metadata")
			}
			return nil
		})
		if err := bcrypt.CompareHashAndPassword(m.Pw, pw); err != nil {
			return errors.NewUnauthorized(err, "Invalid pw")
		}
		return nil
	})
	return err
}

// LoadMetadata
func (r *repo) LoadMetadata(iri pub.IRI) (*storage.Metadata, error) {
	err := r.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	path := itemPath(iri)

	var m *storage.Metadata
	err = r.d.View(func(tx *badger.Txn) error {
		i, err := tx.Get(getMetadataKey(path))
		if err != nil {
			return errors.Annotatef(err, "Could not find metadata in path %s", path)
		}
		m = new(storage.Metadata)
		return i.Value(func(raw []byte) error {
			return json.Unmarshal(raw, m)
		})
	})
	return m, err
}

// SaveMetadata
func (r *repo) SaveMetadata(m storage.Metadata, iri pub.IRI) error {
	err := r.Open()
	if err != nil {
		return err
	}
	defer r.Close()

	path := itemPath(iri)
	err = r.d.Update(func(tx *badger.Txn) error {
		entryBytes, err := jsonld.Marshal(m)
		if err != nil {
			return errors.Annotatef(err, "Could not marshal metadata")
		}
		err = tx.Set(getMetadataKey(path), entryBytes)
		if err != nil {
			return errors.Annotatef(err, "Could not insert entry: %s", path)
		}
		return nil
	})

	return err
}

const objectKey = "__raw"
const metaDataKey = "__meta_data"

func delete(r *repo, it pub.Item) (pub.Item, error) {
	if it.IsCollection() {
		err := pub.OnCollectionIntf(it, func(c pub.CollectionInterface) error {
			var err error
			for _, it := range c.Collection() {
				if it, err = delete(r, it); err != nil {
					return err
				}
			}
			return nil
		})
		return it, err
	}
	f := ap.FiltersNew()
	f.IRI = it.GetLink()
	if it.IsObject() {
		f.Type = ap.CompStrs{ap.StringEquals(string(it.GetType()))}
	}
	old, _ := r.loadOneFromPath(f)

	deleteCollections(r, old)
	t := pub.Tombstone{
		ID:   it.GetLink(),
		Type: pub.TombstoneType,
		To: pub.ItemCollection{
			pub.PublicNS,
		},
		Deleted:    time.Now().UTC(),
		FormerType: old.GetType(),
	}
	return save(r, t)
}

// createCollections
func createCollections(tx *badger.Txn, it pub.Item) error {
	if pub.IsNil(it) || !it.IsObject() {
		return nil
	}
	if pub.ActorTypes.Contains(it.GetType()) {
		pub.OnActor(it, func(p *pub.Actor) error {
			if p.Inbox != nil {
				p.Inbox, _ = createCollectionInPath(tx, p.Inbox)
			}
			if p.Outbox != nil {
				p.Outbox, _ = createCollectionInPath(tx, p.Outbox)
			}
			if p.Followers != nil {
				p.Followers, _ = createCollectionInPath(tx, p.Followers)
			}
			if p.Following != nil {
				p.Following, _ = createCollectionInPath(tx, p.Following)
			}
			if p.Liked != nil {
				p.Liked, _ = createCollectionInPath(tx, p.Liked)
			}
			return nil
		})
	}
	return pub.OnObject(it, func(o *pub.Object) error {
		if o.Replies != nil {
			o.Replies, _ = createCollectionInPath(tx, o.Replies)
		}
		if o.Likes != nil {
			o.Likes, _ = createCollectionInPath(tx, o.Likes)
		}
		if o.Shares != nil {
			o.Shares, _ = createCollectionInPath(tx, o.Shares)
		}
		return nil
	})
}

// deleteCollections
func deleteCollections(r *repo, it pub.Item) error {
	return r.d.Update(func(tx *badger.Txn) error {
		if pub.ActorTypes.Contains(it.GetType()) {
			return pub.OnActor(it, func(p *pub.Actor) error {
				var err error
				err = deleteCollectionFromPath(r, tx, handlers.Inbox.IRI(p))
				err = deleteCollectionFromPath(r, tx, handlers.Outbox.IRI(p))
				err = deleteCollectionFromPath(r, tx, handlers.Followers.IRI(p))
				err = deleteCollectionFromPath(r, tx, handlers.Following.IRI(p))
				err = deleteCollectionFromPath(r, tx, handlers.Liked.IRI(p))
				return err
			})
		}
		if pub.ObjectTypes.Contains(it.GetType()) {
			return pub.OnObject(it, func(o *pub.Object) error {
				var err error
				err = deleteCollectionFromPath(r, tx, handlers.Replies.IRI(o))
				err = deleteCollectionFromPath(r, tx, handlers.Likes.IRI(o))
				err = deleteCollectionFromPath(r, tx, handlers.Shares.IRI(o))
				return err
			})
		}
		return nil
	})
}

func save(r *repo, it pub.Item) (pub.Item, error) {
	itPath := itemPath(it.GetLink())
	err := r.d.Update(func(tx *badger.Txn) error {
		if err := createCollections(tx, it); err != nil {
			return errors.Annotatef(err, "could not create object's collections")
		}
		// TODO(marius): it's possible to set the encoding/decoding functions on the package or storage object level
		//  instead of using jsonld.(Un)Marshal like this.
		entryBytes, err := jsonld.Marshal(it)
		if err != nil {
			return errors.Annotatef(err, "could not marshal object")
		}
		k := getObjectKey(itPath)
		err = tx.Set(k, entryBytes)
		if err != nil {
			return errors.Annotatef(err, "could not store encoded object")
		}

		return nil
	})

	r.cache.Set(it.GetLink(), it)
	return it, err
}

var emptyCollection = []byte{'[', ']'}

func createCollectionInPath(b *badger.Txn, it pub.Item) (pub.Item, error) {
	if pub.IsNil(it) {
		return nil, nil
	}
	p := getObjectKey(itemPath(it.GetLink()))
	err := b.Set(p, emptyCollection)
	if err != nil {
		return nil, err
	}
	return it.GetLink(), nil
}

func deleteCollectionFromPath(r *repo, b *badger.Txn, it pub.Item) error {
	if pub.IsNil(it) {
		return nil
	}
	p := getObjectKey(itemPath(it.GetLink()))
	r.cache.Remove(it.GetLink())
	return b.Delete(p)
}

func (r *repo) loadFromIterator(col *pub.ItemCollection, f s.Filterable) func(val []byte) error {
	isColFn := func(ff s.Filterable) bool {
		_, ok := ff.(pub.IRI)
		return ok
	}
	return func(val []byte) error {
		it, err := loadItem(val)
		if err != nil || pub.IsNil(it) {
			return errors.NewNotFound(err, "not found")
		}
		if !it.IsObject() && it.IsLink() {
			*col, err = r.loadItemsElements(f, it.GetLink())
			return err
		} else if it.IsCollection() {
			return pub.OnCollectionIntf(it, func(c pub.CollectionInterface) error {
				if isColFn(f) {
					f = c.Collection()
				}
				*col, err = r.loadItemsElements(f, c.Collection()...)
				return err
			})
		} else {
			if it.GetType() == pub.CreateType {
				// TODO(marius): this seems terribly not nice
				pub.OnActivity(it, func(a *pub.Activity) error {
					if !a.Object.IsObject() {
						ob, _ := r.loadOneFromPath(a.Object.GetLink())
						a.Object = ob
					}
					return nil
				})
			}

			if pub.IsObject(it) {
				r.cache.Set(it.GetLink(), it)
			}
			it, err = ap.FilterIt(it, f)
			if err != nil {
				return err
			}
			if it != nil {
				*col = append(*col, it)
			}
		}
		return nil
	}
}

var sep = []byte{'/'}

func isObjectKey(k []byte) bool {
	return bytes.HasSuffix(k, []byte(objectKey))
}

func isStorageCollectionKey(p []byte) bool {
	lst := handlers.CollectionType(path.Base(string(p)))
	return ap.FedboxCollections.Contains(lst)
}

func iterKeyIsTooDeep(base, k []byte, depth int) bool {
	res := bytes.TrimPrefix(k, append(base, sep...))
	res = bytes.TrimSuffix(res, []byte(objectKey))
	cnt := bytes.Count(res, sep)
	return cnt > depth
}

func (r *repo) loadFromPath(f s.Filterable) (pub.ItemCollection, uint, error) {
	col := make(pub.ItemCollection, 0)
	err := r.d.View(func(tx *badger.Txn) error {
		iri := f.GetLink()
		fullPath := itemPath(iri)

		depth := 0
		if isStorageCollectionKey(fullPath) {
			depth = 1
		}
		if handlers.ValidCollectionIRI(pub.IRI(fullPath)) {
			depth = 2
		}
		opt := badger.DefaultIteratorOptions
		opt.Prefix = fullPath
		it := tx.NewIterator(opt)
		defer it.Close()
		pathExists := false
		for it.Seek(fullPath); it.ValidForPrefix(fullPath); it.Next() {
			i := it.Item()
			k := i.Key()
			pathExists = true
			if iterKeyIsTooDeep(fullPath, k, depth) {
				continue
			}
			if isObjectKey(k) {
				if cachedIt := r.cache.Get(f.GetLink()); cachedIt != nil {
					col = append(col, cachedIt)
					continue
				}
				if err := i.Value(r.loadFromIterator(&col, f)); err != nil {
					continue
				}
			}
		}
		if !pathExists {
			return errors.NotFoundf("%s does not exist", fullPath)
		}
		return nil
	})

	return col, uint(len(col)), err
}

func (r *repo) LoadOne(f s.Filterable) (pub.Item, error) {
	err := r.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.loadOneFromPath(f)
}

func (r *repo) loadOneFromPath(f s.Filterable) (pub.Item, error) {
	col, cnt, err := r.loadFromPath(f)
	if err != nil {
		return nil, err
	}
	if cnt == 0 {
		return nil, errors.NotFoundf("nothing found")
	}
	return col.First(), nil
}

func getObjectKey(p []byte) []byte {
	return bytes.Join([][]byte{p, []byte(objectKey)}, sep)
}

func (r *repo) loadItemsElements(f s.Filterable, iris ...pub.Item) (pub.ItemCollection, error) {
	col := make(pub.ItemCollection, 0)
	err := r.d.View(func(tx *badger.Txn) error {
		for _, iri := range iris {
			it, err := r.loadItem(tx, itemPath(iri.GetLink()), f)
			if err != nil || pub.IsNil(it) {
				continue
			}
			col = append(col, it)
		}
		return nil
	})
	return col, err
}

func (r *repo) loadItem(b *badger.Txn, path []byte, f s.Filterable) (pub.Item, error) {
	i, err := b.Get(getObjectKey(path))
	if err != nil {
		return nil, errors.NewNotFound(err, "Unable to load path %s", path)
	}
	var raw []byte
	i.Value(func(val []byte) error {
		raw = val
		return nil
	})
	if raw == nil {
		return nil, nil
	}
	var it pub.Item
	it, err = loadItem(raw)
	if err != nil {
		return nil, err
	}
	if pub.IsNil(it) {
		return nil, errors.NotFoundf("not found")
	}
	if it.IsCollection() {
		// we need to dereference them, so no further filtering/processing is needed here
		return it, nil
	}
	if pub.IsIRI(it) {
		it, _ = r.loadOneFromPath(it.GetLink())
	}
	if pub.ActivityTypes.Contains(it.GetType()) {
		pub.OnActivity(it, func(a *pub.Activity) error {
			if it.GetType() == pub.CreateType || ap.FiltersOnActivityObject(f) {
				// TODO(marius): this seems terribly not nice
				if a.Object != nil && !a.Object.IsObject() {
					a.Object, _ = r.loadOneFromPath(a.Object.GetLink())
				}
			}
			if ap.FiltersOnActivityActor(f) {
				// TODO(marius): this seems terribly not nice
				if a.Actor != nil && !a.Actor.IsObject() {
					a.Actor, _ = r.loadOneFromPath(a.Actor.GetLink())
				}
			}
			return nil
		})
	}
	if f != nil {
		return ap.FilterIt(it, f)
	}
	return it, nil
}

func loadItem(raw []byte) (pub.Item, error) {
	if raw == nil || len(raw) == 0 {
		// TODO(marius): log this instead of stopping the iteration and returning an error
		return nil, errors.Errorf("empty raw item")
	}
	return pub.UnmarshalJSON(raw)
}

func itemPath(iri pub.IRI) []byte {
	url, err := iri.URL()
	if err != nil {
		return nil
	}
	return []byte(path.Join(url.Host, url.Path))
}
func (r *repo) CreateService(service pub.Service) error {
	err := r.Open()
	defer r.Close()
	if err != nil {
		return err
	}
	if it, err := save(r, service); err == nil {
		op := "Updated"
		id := it.GetID()
		if !id.IsValid() {
			op = "Added new"
		}
		r.logFn(nil, "%s %s: %s", op, it.GetType(), it.GetLink())
	}
	return err
}

func Path(c Config) (string, error) {
	if c.Path == "" {
		return "", nil
	}
	return c.Path, mkDirIfNotExists(c.Path)
}

func mkDirIfNotExists(p string) error {
	fi, err := os.Stat(p)
	if err != nil && os.IsNotExist(err) {
		err = os.MkdirAll(p, os.ModeDir|os.ModePerm|0700)
	}
	if err != nil {
		return err
	}
	fi, err = os.Stat(p)
	if err != nil {
		return err
	} else if !fi.IsDir() {
		return errors.Errorf("path exists, and is not a folder %s", p)
	}
	return nil
}
