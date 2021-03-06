// +build storage_sqlite storage_all !sqlite_fs,!storage_boltdb,!storage_badger,!storage_pgx

package sqlite

import (
	"database/sql"
	"fmt"
	pub "github.com/go-ap/activitypub"
	"github.com/go-ap/errors"
	ap "github.com/go-ap/fedbox/activitypub"
	"github.com/go-ap/fedbox/internal/cache"
	"github.com/go-ap/fedbox/storage"
	"github.com/go-ap/handlers"
	"github.com/go-ap/jsonld"
	s "github.com/go-ap/storage"
	"golang.org/x/crypto/bcrypt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var encodeFn = jsonld.Marshal
var decodeFn = jsonld.Unmarshal

var errNotImplemented = errors.NotImplementedf("not implemented")

type loggerFn func(string, ...interface{})

var defaultLogFn = func(string, ...interface{}) {}

type Config struct {
	StoragePath string
	BaseURL     string
}

// New returns a new repo repository
func New(c Config) (*repo, error) {
	p, err := getFullPath(c)
	return &repo{
		path:    p,
		baseURL: c.BaseURL,
		logFn:   defaultLogFn,
		errFn:   defaultLogFn,
		cache:   cache.New(true),
	}, err
}

type repo struct {
	conn    *sql.DB
	baseURL string
	path    string
	cache   cache.CanStore
	logFn   loggerFn
	errFn   loggerFn
}

// Open
func (r *repo) Open() error {
	var err error
	r.conn, err = sql.Open("sqlite", r.path)
	return err
}

// Close
func (r *repo) Close() error {
	return r.conn.Close()
}

func (r repo) CreateService(service pub.Service) error {
	err := r.Open()
	defer r.Close()
	if err != nil {
		return err
	}
	it, err := save(r, service)
	if err != nil {
		r.errFn("%s %s: %s", err, it.GetType(), it.GetLink())
	}
	return err
}

func getCollectionTypeFromIRI(i string) handlers.CollectionType {
	col := handlers.CollectionType(path.Base(i))
	if !(ap.FedboxCollections.Contains(col) || handlers.ActivityPubCollections.Contains(col)) {
		b, _ := path.Split(i)
		col = handlers.CollectionType(path.Base(b))
	}
	return getCollectionTable(col)
}

func getCollectionTable(typ handlers.CollectionType) handlers.CollectionType {
	switch typ {
	case handlers.Followers:
		fallthrough
	case handlers.Following:
		fallthrough
	case "actors":
		return "actors"
	case handlers.Inbox:
		fallthrough
	case handlers.Outbox:
		fallthrough
	case handlers.Shares:
		fallthrough
	case handlers.Likes:
		fallthrough
	case "activities":
		return "activities"
	case handlers.Liked:
		fallthrough
	case handlers.Replies:
		fallthrough
	default:
		return "objects"
	}
}

func getCollectionTableFromFilter(f *ap.Filters) handlers.CollectionType {
	return getCollectionTable(f.Collection)
}

// Load
func (r *repo) Load(i pub.IRI) (pub.Item, error) {
	f, err := ap.FiltersFromIRI(i)
	if err != nil {
		return nil, err
	}
	if err = r.Open(); err != nil {
		return nil, err
	}
	defer r.Close()
	return loadFromDb(r, f)
}

// Save
func (r *repo) Save(it pub.Item) (pub.Item, error) {
	if err := r.Open(); err != nil {
		return nil, err
	}
	defer r.Close()
	return save(*r, it)
}

// Create
func (r *repo) Create(col pub.CollectionInterface) (pub.CollectionInterface, error) {
	if col.IsObject() {
		_, err := r.Save(col)
		if err != nil {
			return col, err
		}
	}
	return col, nil
}

// RemoveFrom
func (r *repo) RemoveFrom(col pub.IRI, it pub.Item) error {
	if err := r.Open(); err != nil {
		return err
	}
	defer r.Close()
	query := "DELETE FROM collections where iri = ? AND object = ?;"

	if _, err := r.conn.Exec(query, col, it.GetLink()); err != nil {
		r.errFn("query error: %s\n%s\n%#v", err, query)
		return errors.Annotatef(err, "query error")
	}

	return nil
}

// AddTo
func (r *repo) AddTo(col pub.IRI, it pub.Item) error {
	if err := r.Open(); err != nil {
		return err
	}
	defer r.Close()
	query := "INSERT INTO collections (iri, object) VALUES (?, ?);"

	if _, err := r.conn.Exec(query, col, it.GetLink()); err != nil {
		r.errFn("query error: %s\n%s\n%#v", err, query)
		return errors.Annotatef(err, "query error")
	}

	return nil
}

// Delete
func (r *repo) Delete(it pub.Item) (pub.Item, error) {
	err := r.Open()
	defer r.Close()
	if err != nil {
		return nil, err
	}

	if it.IsCollection() {
		err := pub.OnCollectionIntf(it, func(c pub.CollectionInterface) error {
			var err error
			for _, it := range c.Collection() {
				if it, err = r.Delete(it); err != nil {
					return err
				}
			}
			return nil
		})
		return it, err
	}
	f := ap.FiltersNew()
	f.IRI = it.GetLink()

	t := pub.Tombstone{
		ID:   it.GetLink(),
		Type: pub.TombstoneType,
		To: pub.ItemCollection{
			pub.PublicNS,
		},
		Deleted: time.Now().UTC(),
	}

	if it.IsObject() {
		t.FormerType = it.GetType()
	} else {
		if old, err := loadFromThreeTables(r, f); err == nil {
			t.FormerType = old.GetType()
		}
	}

	//deleteCollections(*r, it)
	return save(*r, t)
}

// PasswordSet
func (r *repo) PasswordSet(it pub.Item, pw []byte) error {
	pw, err := bcrypt.GenerateFromPassword(pw, -1)
	if err != nil {
		return errors.Annotatef(err, "could not generate pw hash")
	}
	m := storage.Metadata{
		Pw: pw,
	}
	return r.SaveMetadata(m, it.GetLink())
}

// PasswordCheck
func (r *repo) PasswordCheck(it pub.Item, pw []byte) error {
	m, err := r.LoadMetadata(it.GetLink())
	if err != nil {
		return errors.Annotatef(err, "Could not find load metadata for %s", it)
	}
	if err := bcrypt.CompareHashAndPassword(m.Pw, pw); err != nil {
		return errors.NewUnauthorized(err, "Invalid pw")
	}
	return err
}

// LoadMetadata
func (r *repo) LoadMetadata(iri pub.IRI) (*storage.Metadata, error) {
	err := r.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()

	m := new(storage.Metadata)
	raw, err := loadMetadataFromTable(r.conn, iri)
	if err != nil {
		return nil, err
	}
	err = decodeFn(raw, m)
	if err != nil {
		return nil, errors.Annotatef(err, "Could not unmarshal metadata")
	}
	return m, nil
}

// SaveMetadata
func (r *repo) SaveMetadata(m storage.Metadata, iri pub.IRI) error {
	err := r.Open()
	if err != nil {
		return err
	}
	defer r.Close()

	entryBytes, err := encodeFn(m)
	if err != nil {
		return errors.Annotatef(err, "Could not marshal metadata")
	}
	return saveMetadataToTable(r.conn, iri, entryBytes)
}

func getFullPath(c Config) (string, error) {
	p, err := getAbsStoragePath(c.StoragePath)
	if err != nil {
		return "memory", err
	}
	if err := mkDirIfNotExists(p); err != nil {
		return "memory", err
	}
	return path.Join(p, "storage.sqlite"), nil
}

func getAbsStoragePath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		var err error
		p, err = filepath.Abs(p)
		if err != nil {
			return "", err
		}
	}
	if fi, err := os.Stat(p); err != nil {
		return "", err
	} else if !fi.IsDir() {
		return "", errors.NotValidf("path %s is invalid for storage", p)
	}
	return p, nil
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

func saveMetadataToTable(conn *sql.DB, iri pub.IRI, m []byte) error {
	table := getCollectionTypeFromIRI(iri.String())

	query := fmt.Sprintf("UPDATE %s SET meta = ? WHERE iri = ?;", table)
	_, err := conn.Exec(query, m, iri)
	return err
}

func loadMetadataFromTable(conn *sql.DB, iri pub.IRI) ([]byte, error) {
	table := getCollectionTypeFromIRI(iri.String())

	var meta []byte
	sel := fmt.Sprintf("SELECT meta FROM %s WHERE iri = ?;", table)
	err := conn.QueryRow(sel, iri).Scan(&meta)
	return meta, err
}

func isSingleItem(f s.Filterable) bool {
	if _, isIRI := f.(pub.IRI); isIRI {
		return true
	}
	if _, isItem := f.(pub.Item); isItem {
		return true
	}
	return false
}

func loadFromObjects(r *repo, f *ap.Filters) (pub.ItemCollection, error) {
	return loadFromOneTable(r, "objects", f)
}
func loadFromActors(r *repo, f *ap.Filters) (pub.ItemCollection, error) {
	return loadFromOneTable(r, "actors", f)
}
func loadFromActivities(r *repo, f *ap.Filters) (pub.ItemCollection, error) {
	return loadFromOneTable(r, "activities", f)
}

func loadFromThreeTables(r *repo, f *ap.Filters) (pub.ItemCollection, error) {
	result := make(pub.ItemCollection, 0)
	if obj, err := loadFromObjects(r, f); err == nil {
		result = append(result, obj...)
	}
	if actors, err := loadFromActors(r, f); err == nil {
		result = append(result, actors...)
	}
	if activities, err := loadFromActors(r, f); err == nil {
		result = append(result, activities...)
	}
	return result, nil
}

func loadFromOneTable(r *repo, table handlers.CollectionType, f *ap.Filters) (pub.ItemCollection, error) {
	conn := r.conn
	// NOTE(marius): this doesn't seem to be working, our filter is never an IRI or Item
	if isSingleItem(f) {
		if cachedIt := r.cache.Get(f.GetLink()); cachedIt != nil {
			return pub.ItemCollection{cachedIt}, nil
		}
	}
	if table == "" {
		table = getCollectionTableFromFilter(f)
	}
	clauses, values := getWhereClauses(f)
	var total uint = 0

	selCnt := fmt.Sprintf("SELECT COUNT(id) FROM %s WHERE %s", table, strings.Join(clauses, " AND "))
	if err := conn.QueryRow(selCnt, values...).Scan(&total); err != nil {
		return nil, errors.Annotatef(err, "unable to count all rows")
	}
	ret := make(pub.ItemCollection, 0)
	if total == 0 {
		return ret, nil
	}

	sel := fmt.Sprintf("SELECT id, iri, published, type, raw FROM %s WHERE %s ORDER BY published %s", table, strings.Join(clauses, " AND "), getLimit(f))
	rows, err := conn.Query(sel, values...)
	if err != nil {
		if err == sql.ErrNoRows {
			return pub.ItemCollection{}, nil
		}
		return nil, errors.Annotatef(err, "unable to run select")
	}

	// Iterate through the result set
	for rows.Next() {
		var id int64
		var iri string
		var created string
		var typ string
		var raw []byte
		err = rows.Scan(&id, &iri, &created, &typ, &raw)
		if err != nil {
			return ret, errors.Annotatef(err, "scan values error")
		}

		it, err := pub.UnmarshalJSON(raw)
		if err != nil {
			return ret, errors.Annotatef(err, "unable to unmarshal raw item")
		}
		if pub.IsObject(it) {
			r.cache.Set(it.GetLink(), it)
		}
		ret = append(ret, it)
	}

	ret = runActivityFilters(r, ret, f)
	return ret, err
}

func runActivityFilters(r *repo, ret pub.ItemCollection, f *ap.Filters) pub.ItemCollection {
	// If our filter contains values for filtering the activity's object or actor, we do that here:
	//  for the case where the corresponding values are not set, this doesn't do anything
	toRemove := make([]int, 0)
	toRemove = append(toRemove, childFilter(r, &ret, f.Object, func(act pub.Item, ob pub.Item) bool {
		var keep bool
		pub.OnActivity(act, func(a *pub.Activity) error {
			if a.Object.GetLink().Equals(ob.GetLink(), false) {
				a.Object = ob
				keep = true
			}
			return nil
		})
		return keep
	})...)
	toRemove = append(toRemove, childFilter(r, &ret, f.Actor, func(act pub.Item, ob pub.Item) bool {
		var keep bool
		pub.OnActivity(act, func(a *pub.Activity) error {
			if a.Actor.GetLink().Equals(ob.GetLink(), false) {
				a.Actor = ob
				keep = true
			}
			return nil
		})
		return keep
	})...)
	toRemove = append(toRemove, childFilter(r, &ret, f.Target, func(act pub.Item, ob pub.Item) bool {
		var keep bool
		pub.OnActivity(act, func(a *pub.Activity) error {
			if a.Target.GetLink().Equals(ob.GetLink(), false) {
				a.Target = ob
				keep = true
			}
			return nil
		})
		return keep
	})...)

	result := make(pub.ItemCollection, 0)
	for i := range ret {
		keep := true
		for _, id := range toRemove {
			if i == id {
				keep = false
			}
		}
		if keep {
			result = append(result, ret[i])
		}
	}
	return result
}

func childFilter(r *repo, ret *pub.ItemCollection, f *ap.Filters, keepFn func(act, ob pub.Item) bool) []int {
	if f == nil {
		return nil
	}
	toRemove := make([]int, 0)
	children, _ := loadFromThreeTables(r, f)
	for i, rr := range *ret {
		if !pub.ActivityTypes.Contains(rr.GetType()) {
			toRemove = append(toRemove, i)
			continue
		}
		keep := false
		for _, ob := range children {
			keep = keepFn(rr, ob)
			if keep {
				break
			}
		}
		if !keep {
			toRemove = append(toRemove, i)
		}
	}
	return toRemove
}

func loadFromDb(r *repo, f *ap.Filters) (pub.Item, error) {
	conn := r.conn
	table := getCollectionTableFromFilter(f)
	clauses, values := getWhereClauses(f)
	var total uint = 0

	// todo(marius): this needs to be split into three cases:
	//  1. IRI corresponds to a collection that is not one of the storage tables (ie, not activities, actors, objects):
	//    Then we look for correspondences in the collections table.
	//  2. The IRI corresponds to the activities, actors, objects tables:
	//    Then we load from the corresponding table using `iri LIKE IRI%` criteria
	//  3. IRI corresponds to an object: we load directly from the corresponding table.
	selCnt := fmt.Sprintf("SELECT COUNT(id) FROM %s WHERE %s", table, strings.Join(clauses, " AND "))
	if err := conn.QueryRow(selCnt, values...).Scan(&total); err != nil && err != sql.ErrNoRows {
		return nil, errors.Annotatef(err, "unable to count all rows")
	}
	if total > 0 {
		return loadFromOneTable(r, table, f)
	}
	var (
		iriClause string
		iriValue  interface{}
		hasIRI    = false
	)
	valIdx := -1
	for _, c := range clauses {
		valIdx += strings.Count(c, "?")
		if strings.Contains(c, "iri") {
			iriClause = c
			iriValue = values[valIdx]
			hasIRI = true
		}
	}
	if !hasIRI {
		return nil, errors.NotFoundf("Not found")
	}
	colCntQ := fmt.Sprintf("SELECT COUNT(id) FROM %s WHERE %s", "collections", iriClause)
	if err := conn.QueryRow(colCntQ, iriValue).Scan(&total); err != nil && err != sql.ErrNoRows {
		return nil, errors.Annotatef(err, "unable to count all rows")
	}
	if total == 0 && handlers.ActivityPubCollections.Contains(f.Collection) && !MandatoryCollections.Contains(f.Collection) {
		return nil, errors.NotFoundf("Unable to find collection %s", f.Collection)
	}
	sel := fmt.Sprintf("SELECT id, iri, object FROM %s WHERE %s %s", "collections", iriClause, getLimit(f))
	rows, err := conn.Query(sel, iriValue)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, errors.NotFoundf("Unable to load %s", f.Collection)
		}
		return nil, errors.Annotatef(err, "unable to run select")
	}
	fOb := *f
	fActors := *f
	fActivities := *f

	fOb.IRI = ""
	fOb.Collection = "objects"
	fOb.ItemKey = make(ap.CompStrs, 0)
	fActors.IRI = ""
	fActors.Collection = "actors"
	fActors.ItemKey = make(ap.CompStrs, 0)
	fActivities.IRI = ""
	fActivities.Collection = "activities"
	fActivities.ItemKey = make(ap.CompStrs, 0)
	// Iterate through the result set
	for rows.Next() {
		var id int64
		var object string
		var iri string

		err = rows.Scan(&id, &iri, &object)
		if err != nil {
			return pub.ItemCollection{}, errors.Annotatef(err, "scan values error")
		}
		col := getCollectionTypeFromIRI(iri)
		if col == "objects" {
			fOb.ItemKey = append(fOb.ItemKey, ap.StringEquals(object))
		} else if col == "actors" {
			fActors.ItemKey = append(fActors.ItemKey, ap.StringEquals(object))
		} else if col == "activities" {
			fActivities.ItemKey = append(fActivities.ItemKey, ap.StringEquals(object))
		} else {
			switch table {
			case "activities":
				fActivities.ItemKey = append(fActivities.ItemKey, ap.StringEquals(object))
			case "actors":
				fActors.ItemKey = append(fActors.ItemKey, ap.StringEquals(object))
			case "objects":
				fallthrough
			default:
				fOb.ItemKey = append(fOb.ItemKey, ap.StringEquals(object))
			}
		}
	}
	ret := make(pub.ItemCollection, 0)
	if len(fActivities.ItemKey) > 0 {
		retAct, err := loadFromActivities(r, &fActivities)
		if err != nil {
			return ret, err
		}
		ret = append(ret, retAct...)
	}
	if len(fActors.ItemKey) > 0 {
		retAct, err := loadFromActors(r, &fActors)
		if err != nil {
			return ret, err
		}
		ret = append(ret, retAct...)
	}
	if len(fOb.ItemKey) > 0 {
		retOb, err := loadFromObjects(r, &fOb)
		if err != nil {
			return ret, err
		}
		ret = append(ret, retOb...)
	}
	return ret, nil
}

func save(l repo, it pub.Item) (pub.Item, error) {
	iri := it.GetLink()

	if err := flattenCollections(it); err != nil {
		return it, errors.Annotatef(err, "could not create object's collections")
	}
	raw, err := encodeFn(it)
	if err != nil {
		l.errFn("query error: %s", err)
		return it, errors.Annotatef(err, "query error")
	}

	columns := []string{
		"iri",
		"published",
		"type",
		"raw",
	}
	tokens := []string{"?", "?", "?", "?"}
	params := []interface{}{
		interface{}(iri),
		interface{}(time.Now().UTC()),
		interface{}(it.GetType()),
		interface{}(raw),
	}

	table := string(ap.ObjectsType)
	pub.OnObject(it, func(o *pub.Object) error {
		if o.URL != nil {
			columns = append(columns, "url")
			tokens = append(tokens, "?")
			params = append(params, interface{}(o.URL.GetLink()))
		}
		if o.Name.Count() > 0 {
			columns = append(columns, "name")
			tokens = append(tokens, "?")
			params = append(params, interface{}(o.Name.String()))
		}
		rec := o.Recipients()
		if rec.Count() > 0 {
			if raw, err := encodeFn(rec); err == nil {
				columns = append(columns, "audience")
				tokens = append(tokens, "?")
				params = append(params, interface{}(raw))
			}
		}
		if pub.ObjectTypes.Contains(o.Type) {
			if o.Summary.Count() > 0 {
				columns = append(columns, "summary")
				tokens = append(tokens, "?")
				params = append(params, interface{}(o.Summary.String()))
			}
		}
		if !pub.ActorTypes.Contains(o.Type) {
			if o.Content.Count() > 0 {
				columns = append(columns, "content")
				tokens = append(tokens, "?")
				params = append(params, interface{}(o.Content.String()))
			}
		}
		return nil
	})
	if pub.ActivityTypes.Contains(it.GetType()) {
		table = string(ap.ActivitiesType)
		pub.OnActivity(it, func(a *pub.Activity) error {
			columns = append(columns, "actor")
			tokens = append(tokens, "?")
			params = append(params, interface{}(a.Actor.GetLink()))

			columns = append(columns, "object")
			tokens = append(tokens, "?")
			params = append(params, interface{}(a.Object.GetLink()))
			return nil
		})
	} else if pub.ActorTypes.Contains(it.GetType()) {
		table = string(ap.ActorsType)
		pub.OnActor(it, func(a *pub.Actor) error {
			return nil
		})
	} else if it.GetType() == pub.TombstoneType {
		if strings.Contains(iri.String(), string(ap.ActorsType)) {
			table = string(ap.ActorsType)
			pub.OnActor(it, func(a *pub.Actor) error {
				if a.PreferredUsername.Count() > 0 {
					columns = append(columns, "preferred_username")
					tokens = append(tokens, "?")
					params = append(params, interface{}(a.PreferredUsername.String()))
				}
				return nil
			})
		}
		if strings.Contains(iri.String(), string(ap.ActivitiesType)) {
			table = string(ap.ActivitiesType)
			pub.OnActivity(it, func(a *pub.Activity) error {
				return nil
			})
		}
	}

	query := fmt.Sprintf("INSERT OR REPLACE INTO %s (%s) VALUES (%s);", table, strings.Join(columns, ", "), strings.Join(tokens, ", "))

	if _, err = l.conn.Exec(query, params...); err != nil {
		l.errFn("query error: %s\n%s", err, query)
		return it, errors.Annotatef(err, "query error")
	}
	col, key := path.Split(iri.String())
	if len(key) > 0 && handlers.ValidCollection(handlers.CollectionType(path.Base(col))) {
		// Add private items to the collections table
		if colIRI, k := handlers.Split(pub.IRI(col)); k == "" {
			if err := l.AddTo(colIRI, it); err != nil {
				return it, err
			}
		}
	}

	l.cache.Set(it.GetLink(), it)
	return it, nil
}

// flattenCollections
func flattenCollections(it pub.Item) error {
	if pub.IsNil(it) || !it.IsObject() {
		return nil
	}
	if pub.ActorTypes.Contains(it.GetType()) {
		pub.OnActor(it, func(p *pub.Actor) error {
			p.Inbox = pub.FlattenToIRI(p.Inbox)
			p.Outbox = pub.FlattenToIRI(p.Outbox)
			p.Followers = pub.FlattenToIRI(p.Followers)
			p.Following = pub.FlattenToIRI(p.Following)
			p.Liked = pub.FlattenToIRI(p.Liked)
			return nil
		})
	}
	return pub.OnObject(it, func(o *pub.Object) error {
		o.Replies = pub.FlattenToIRI(o.Replies)
		o.Likes = pub.FlattenToIRI(o.Likes)
		o.Shares = pub.FlattenToIRI(o.Shares)
		return nil
	})
}
