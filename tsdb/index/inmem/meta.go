package inmem

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/query"
	"github.com/influxdata/influxdb/tsdb"
	"github.com/influxdata/influxql"
)

// Measurement represents a collection of time series in a database. It also
// contains in memory structures for indexing tags. Exported functions are
// goroutine safe while un-exported functions assume the caller will use the
// appropriate locks.
type Measurement struct {
	database string
	Name     string `json:"name,omitempty"`
	name     []byte // cached version as []byte

	mu         sync.RWMutex
	fieldNames map[string]struct{}

	// in-memory index fields
	seriesByID          map[uint64]*Series      // lookup table for series by their id
	seriesByTagKeyValue map[string]*TagKeyValue // map from tag key to value to sorted set of series ids

	// lazyily created sorted series IDs
	sortedSeriesIDs SeriesIDs // sorted list of series IDs in this measurement

	// Indicates whether the seriesByTagKeyValueMap needs to be rebuilt as it contains deleted series
	// that waste memory.
	dirty bool
}

// NewMeasurement allocates and initializes a new Measurement.
func NewMeasurement(database, name string) *Measurement {
	return &Measurement{
		database:   database,
		Name:       name,
		name:       []byte(name),
		fieldNames: make(map[string]struct{}),

		seriesByID:          make(map[uint64]*Series),
		seriesByTagKeyValue: make(map[string]*TagKeyValue),
	}
}

// Authorized determines if this Measurement is authorized to be read, according
// to the provided Authorizer. A measurement is authorized to be read if at
// least one series from the measurement is authorized to be read.
func (m *Measurement) Authorized(auth query.Authorizer) bool {
	if auth == nil {
		return true
	}

	// Note(edd): the cost of this check scales linearly with the number of series
	// belonging to a measurement, which means it may become expensive when there
	// are large numbers of series on a measurement.
	//
	// In the future we might want to push the set of series down into the
	// authorizer, but that will require an API change.
	for _, s := range m.SeriesByIDMap() {
		if auth.AuthorizeSeriesRead(m.database, m.name, s.tags) {
			return true
		}
	}
	return false
}

func (m *Measurement) HasField(name string) bool {
	m.mu.RLock()
	_, hasField := m.fieldNames[name]
	m.mu.RUnlock()
	return hasField
}

// SeriesByID returns a series by identifier.
func (m *Measurement) SeriesByID(id uint64) *Series {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.seriesByID[id]
}

// SeriesByIDMap returns the internal seriesByID map.
func (m *Measurement) SeriesByIDMap() map[uint64]*Series {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.seriesByID
}

// SeriesByIDSlice returns a list of series by identifiers.
func (m *Measurement) SeriesByIDSlice(ids []uint64) []*Series {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := make([]*Series, len(ids))
	for i, id := range ids {
		a[i] = m.seriesByID[id]
	}
	return a
}

// AppendSeriesKeysByID appends keys for a list of series ids to a buffer.
func (m *Measurement) AppendSeriesKeysByID(dst []string, ids []uint64) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range ids {
		if s := m.seriesByID[id]; s != nil && !s.Deleted() {
			dst = append(dst, s.Key)
		}
	}
	return dst
}

// SeriesKeysByID returns the a list of keys for a set of ids.
func (m *Measurement) SeriesKeysByID(ids SeriesIDs) [][]byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([][]byte, 0, len(ids))
	for _, id := range ids {
		s := m.seriesByID[id]
		if s == nil || s.Deleted() {
			continue
		}
		keys = append(keys, []byte(s.Key))
	}
	return keys
}

// SeriesKeys returns the keys of every series in this measurement
func (m *Measurement) SeriesKeys() [][]byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([][]byte, 0, len(m.seriesByID))
	for _, s := range m.seriesByID {
		if s.Deleted() {
			continue
		}
		keys = append(keys, []byte(s.Key))
	}
	return keys
}

func (m *Measurement) SeriesIDs() SeriesIDs {
	m.mu.RLock()
	if len(m.sortedSeriesIDs) == len(m.seriesByID) {
		s := m.sortedSeriesIDs
		m.mu.RUnlock()
		return s
	}
	m.mu.RUnlock()

	m.mu.Lock()
	if len(m.sortedSeriesIDs) == len(m.seriesByID) {
		s := m.sortedSeriesIDs
		m.mu.Unlock()
		return s
	}

	m.sortedSeriesIDs = m.sortedSeriesIDs[:0]
	if cap(m.sortedSeriesIDs) < len(m.seriesByID) {
		m.sortedSeriesIDs = make(SeriesIDs, 0, len(m.seriesByID))
	}

	for k, v := range m.seriesByID {
		if v.Deleted() {
			continue
		}
		m.sortedSeriesIDs = append(m.sortedSeriesIDs, k)
	}
	sort.Sort(m.sortedSeriesIDs)
	s := m.sortedSeriesIDs
	m.mu.Unlock()
	return s
}

// HasTagKey returns true if at least one series in this measurement has written a value for the passed in tag key
func (m *Measurement) HasTagKey(k string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, hasTag := m.seriesByTagKeyValue[k]
	return hasTag
}

func (m *Measurement) HasTagKeyValue(k, v []byte) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.seriesByTagKeyValue[string(k)].Contains(string(v))
}

// HasSeries returns true if there is at least 1 series under this measurement.
func (m *Measurement) HasSeries() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.seriesByID) > 0
}

// Cardinality returns the number of values associated with the given tag key.
func (m *Measurement) Cardinality(key string) int {
	var n int
	m.mu.RLock()
	n = m.cardinality(key)
	m.mu.RUnlock()
	return n
}

func (m *Measurement) cardinality(key string) int {
	return m.seriesByTagKeyValue[key].Cardinality()
}

// CardinalityBytes returns the number of values associated with the given tag key.
func (m *Measurement) CardinalityBytes(key []byte) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.seriesByTagKeyValue[string(key)].Cardinality()
}

// AddSeries adds a series to the measurement's index.
// It returns true if the series was added successfully or false if the series was already present.
func (m *Measurement) AddSeries(s *Series) bool {
	if s == nil {
		return false
	}

	m.mu.RLock()
	if m.seriesByID[s.ID] != nil {
		m.mu.RUnlock()
		return false
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.seriesByID[s.ID] != nil {
		return false
	}

	m.seriesByID[s.ID] = s

	if len(m.seriesByID) == 1 || (len(m.sortedSeriesIDs) == len(m.seriesByID)-1 && s.ID > m.sortedSeriesIDs[len(m.sortedSeriesIDs)-1]) {
		m.sortedSeriesIDs = append(m.sortedSeriesIDs, s.ID)
	}

	// add this series id to the tag index on the measurement
	s.ForEachTag(func(t models.Tag) {
		valueMap := m.seriesByTagKeyValue[string(t.Key)]
		if valueMap == nil {
			valueMap = NewTagKeyValue()
			m.seriesByTagKeyValue[string(t.Key)] = valueMap
		}
		ids := valueMap.LoadByte(t.Value)
		ids = append(ids, s.ID)

		// most of the time the series ID will be higher than all others because it's a new
		// series. So don't do the sort if we don't have to.
		if len(ids) > 1 && ids[len(ids)-1] < ids[len(ids)-2] {
			sort.Sort(ids)
		}
		valueMap.Store(string(t.Value), ids)
	})

	return true
}

// DropSeries removes a series from the measurement's index.
func (m *Measurement) DropSeries(series *Series) {
	seriesID := series.ID
	m.mu.Lock()
	defer m.mu.Unlock()

	// Existence check before delete here to clean up the caching/indexing only when needed
	if _, ok := m.seriesByID[seriesID]; !ok {
		return
	}
	delete(m.seriesByID, seriesID)

	// clear our lazily sorted set of ids
	m.sortedSeriesIDs = m.sortedSeriesIDs[:0]

	// Mark that this measurements tagValue map has stale entries that need to be rebuilt.
	m.dirty = true
}

func (m *Measurement) Rebuild() *Measurement {
	m.mu.RLock()

	// Nothing needs to be rebuilt.
	if !m.dirty {
		m.mu.RUnlock()
		return m
	}

	// Create a new measurement from the state of the existing measurement
	nm := NewMeasurement(m.database, string(m.name))
	nm.fieldNames = m.fieldNames
	m.mu.RUnlock()

	// Re-add each series to allow the measurement indexes to get re-created.  If there were
	// deletes, the existing measurment may have references to deleted series that need to be
	// expunged.  Note: we're NOT using SeriesIDs which returns the series in sorted order because
	// we need to do this under a write lock to prevent races.  The series are added in sorted
	// order to prevent resorting them again after they are all re-added.
	m.mu.Lock()
	defer m.mu.Unlock()

	for k, v := range m.seriesByID {
		if v.Deleted() {
			continue
		}
		m.sortedSeriesIDs = append(m.sortedSeriesIDs, k)
	}
	sort.Sort(m.sortedSeriesIDs)

	for _, id := range m.sortedSeriesIDs {
		if s := m.seriesByID[id]; s != nil {
			nm.AddSeries(s)
		}
	}
	return nm
}

// filters walks the where clause of a select statement and returns a map with all series ids
// matching the where clause and any filter expression that should be applied to each
func (m *Measurement) filters(condition influxql.Expr) ([]uint64, map[uint64]influxql.Expr, error) {
	if condition == nil {
		return m.SeriesIDs(), nil, nil
	}
	return m.WalkWhereForSeriesIds(condition)
}

// ForEachSeriesByExpr iterates over all series filtered by condition.
func (m *Measurement) ForEachSeriesByExpr(condition influxql.Expr, fn func(tags models.Tags) error) error {
	// Retrieve matching series ids.
	ids, _, err := m.filters(condition)
	if err != nil {
		return err
	}

	// Iterate over each series.
	for _, id := range ids {
		s := m.SeriesByID(id)
		if err := fn(s.Tags()); err != nil {
			return err
		}
	}

	return nil
}

// TagSets returns the unique tag sets that exist for the given tag keys. This is used to determine
// what composite series will be created by a group by. i.e. "group by region" should return:
// {"region":"uswest"}, {"region":"useast"}
// or region, service returns
// {"region": "uswest", "service": "redis"}, {"region": "uswest", "service": "mysql"}, etc...
// This will also populate the TagSet objects with the series IDs that match each tagset and any
// influx filter expression that goes with the series
// TODO: this shouldn't be exported. However, until tx.go and the engine get refactored into tsdb, we need it.
func (m *Measurement) TagSets(shardID uint64, opt query.IteratorOptions) ([]*query.TagSet, error) {
	// get the unique set of series ids and the filters that should be applied to each
	ids, filters, err := m.filters(opt.Condition)
	if err != nil {
		return nil, err
	}

	var dims []string
	if len(opt.Dimensions) > 0 {
		dims = make([]string, len(opt.Dimensions))
		copy(dims, opt.Dimensions)
		sort.Strings(dims)
	}

	m.mu.RLock()
	// For every series, get the tag values for the requested tag keys i.e. dimensions. This is the
	// TagSet for that series. Series with the same TagSet are then grouped together, because for the
	// purpose of GROUP BY they are part of the same composite series.
	tagSets := make(map[string]*query.TagSet, 64)
	var seriesN int
	for _, id := range ids {
		// Abort if the query was killed
		select {
		case <-opt.InterruptCh:
			m.mu.RUnlock()
			return nil, query.ErrQueryInterrupted
		default:
		}

		if opt.MaxSeriesN > 0 && seriesN > opt.MaxSeriesN {
			m.mu.RUnlock()
			return nil, fmt.Errorf("max-select-series limit exceeded: (%d/%d)", seriesN, opt.MaxSeriesN)
		}

		s := m.seriesByID[id]
		if s == nil || s.Deleted() || !s.Assigned(shardID) {
			continue
		}

		if opt.Authorizer != nil && !opt.Authorizer.AuthorizeSeriesRead(m.database, m.name, s.Tags()) {
			continue
		}

		var tagsAsKey []byte
		if len(dims) > 0 {
			tagsAsKey = tsdb.MakeTagsKey(dims, s.Tags())
		}

		tagSet := tagSets[string(tagsAsKey)]
		if tagSet == nil {
			// This TagSet is new, create a new entry for it.
			tagSet = &query.TagSet{
				Tags: nil,
				Key:  tagsAsKey,
			}
			tagSets[string(tagsAsKey)] = tagSet
		}
		// Associate the series and filter with the Tagset.
		tagSet.AddFilter(s.Key, filters[id])
		seriesN++
	}
	// Release the lock while we sort all the tags
	m.mu.RUnlock()

	// Sort the series in each tag set.
	for _, t := range tagSets {
		// Abort if the query was killed
		select {
		case <-opt.InterruptCh:
			return nil, query.ErrQueryInterrupted
		default:
		}

		sort.Sort(t)
	}

	// The TagSets have been created, as a map of TagSets. Just send
	// the values back as a slice, sorting for consistency.
	sortedTagsSets := make([]*query.TagSet, 0, len(tagSets))
	for _, v := range tagSets {
		sortedTagsSets = append(sortedTagsSets, v)
	}
	sort.Sort(byTagKey(sortedTagsSets))

	return sortedTagsSets, nil
}

// intersectSeriesFilters performs an intersection for two sets of ids and filter expressions.
func intersectSeriesFilters(lids, rids SeriesIDs, lfilters, rfilters FilterExprs) (SeriesIDs, FilterExprs) {
	// We only want to allocate a slice and map of the smaller size.
	var ids []uint64
	if len(lids) > len(rids) {
		ids = make([]uint64, 0, len(rids))
	} else {
		ids = make([]uint64, 0, len(lids))
	}

	var filters FilterExprs
	if len(lfilters) > len(rfilters) {
		filters = make(FilterExprs, len(rfilters))
	} else {
		filters = make(FilterExprs, len(lfilters))
	}

	// They're in sorted order so advance the counter as needed.
	// This is, don't run comparisons against lower values that we've already passed.
	for len(lids) > 0 && len(rids) > 0 {
		lid, rid := lids[0], rids[0]
		if lid == rid {
			ids = append(ids, lid)

			var expr influxql.Expr
			lfilter := lfilters[lid]
			rfilter := rfilters[rid]

			if lfilter != nil && rfilter != nil {
				be := &influxql.BinaryExpr{
					Op:  influxql.AND,
					LHS: lfilter,
					RHS: rfilter,
				}
				expr = influxql.Reduce(be, nil)
			} else if lfilter != nil {
				expr = lfilter
			} else if rfilter != nil {
				expr = rfilter
			}

			if expr != nil {
				filters[lid] = expr
			}
			lids, rids = lids[1:], rids[1:]
		} else if lid < rid {
			lids = lids[1:]
		} else {
			rids = rids[1:]
		}
	}
	return ids, filters
}

// unionSeriesFilters performs a union for two sets of ids and filter expressions.
func unionSeriesFilters(lids, rids SeriesIDs, lfilters, rfilters FilterExprs) (SeriesIDs, FilterExprs) {
	ids := make([]uint64, 0, len(lids)+len(rids))

	// Setup the filters with the smallest size since we will discard filters
	// that do not have a match on the other side.
	var filters FilterExprs
	if len(lfilters) < len(rfilters) {
		filters = make(FilterExprs, len(lfilters))
	} else {
		filters = make(FilterExprs, len(rfilters))
	}

	for len(lids) > 0 && len(rids) > 0 {
		lid, rid := lids[0], rids[0]
		if lid == rid {
			ids = append(ids, lid)

			// If one side does not have a filter, then the series has been
			// included on one side of the OR with no condition. Eliminate the
			// filter in this case.
			var expr influxql.Expr
			lfilter := lfilters[lid]
			rfilter := rfilters[rid]
			if lfilter != nil && rfilter != nil {
				be := &influxql.BinaryExpr{
					Op:  influxql.OR,
					LHS: lfilter,
					RHS: rfilter,
				}
				expr = influxql.Reduce(be, nil)
			}

			if expr != nil {
				filters[lid] = expr
			}
			lids, rids = lids[1:], rids[1:]
		} else if lid < rid {
			ids = append(ids, lid)

			filter := lfilters[lid]
			if filter != nil {
				filters[lid] = filter
			}
			lids = lids[1:]
		} else {
			ids = append(ids, rid)

			filter := rfilters[rid]
			if filter != nil {
				filters[rid] = filter
			}
			rids = rids[1:]
		}
	}

	// Now append the remainder.
	if len(lids) > 0 {
		for i := 0; i < len(lids); i++ {
			ids = append(ids, lids[i])

			filter := lfilters[lids[i]]
			if filter != nil {
				filters[lids[i]] = filter
			}
		}
	} else if len(rids) > 0 {
		for i := 0; i < len(rids); i++ {
			ids = append(ids, rids[i])

			filter := rfilters[rids[i]]
			if filter != nil {
				filters[rids[i]] = filter
			}
		}
	}
	return ids, filters
}

// IDsForExpr returns the series IDs that are candidates to match the given expression.
func (m *Measurement) IDsForExpr(n *influxql.BinaryExpr) SeriesIDs {
	ids, _, _ := m.idsForExpr(n)
	return ids
}

// idsForExpr returns a collection of series ids and a filter expression that should
// be used to filter points from those series.
func (m *Measurement) idsForExpr(n *influxql.BinaryExpr) (SeriesIDs, influxql.Expr, error) {
	// If this binary expression has another binary expression, then this
	// is some expression math and we should just pass it to the underlying query.
	if _, ok := n.LHS.(*influxql.BinaryExpr); ok {
		return m.SeriesIDs(), n, nil
	} else if _, ok := n.RHS.(*influxql.BinaryExpr); ok {
		return m.SeriesIDs(), n, nil
	}

	// Retrieve the variable reference from the correct side of the expression.
	name, ok := n.LHS.(*influxql.VarRef)
	value := n.RHS
	if !ok {
		name, ok = n.RHS.(*influxql.VarRef)
		if !ok {
			return nil, nil, fmt.Errorf("invalid expression: %s", n.String())
		}
		value = n.LHS
	}

	// For fields, return all series IDs from this measurement and return
	// the expression passed in, as the filter.
	if name.Val != "_name" && ((name.Type == influxql.Unknown && m.HasField(name.Val)) || name.Type == influxql.AnyField || (name.Type != influxql.Tag && name.Type != influxql.Unknown)) {
		return m.SeriesIDs(), n, nil
	} else if value, ok := value.(*influxql.VarRef); ok {
		// Check if the RHS is a variable and if it is a field.
		if value.Val != "_name" && ((value.Type == influxql.Unknown && m.HasField(value.Val)) || name.Type == influxql.AnyField || (value.Type != influxql.Tag && value.Type != influxql.Unknown)) {
			return m.SeriesIDs(), n, nil
		}
	}

	// Retrieve list of series with this tag key.
	tagVals := m.seriesByTagKeyValue[name.Val]

	// if we're looking for series with a specific tag value
	if str, ok := value.(*influxql.StringLiteral); ok {
		var ids SeriesIDs

		// Special handling for "_name" to match measurement name.
		if name.Val == "_name" {
			if (n.Op == influxql.EQ && str.Val == m.Name) || (n.Op == influxql.NEQ && str.Val != m.Name) {
				return m.SeriesIDs(), nil, nil
			}
			return nil, nil, nil
		}

		if n.Op == influxql.EQ {
			if str.Val != "" {
				// return series that have a tag of specific value.
				ids = tagVals.Load(str.Val)
			} else {
				// Make a copy of all series ids and mark the ones we need to evict.
				seriesIDs := newEvictSeriesIDs(m.SeriesIDs())

				// Go through each slice and mark the values we find as zero so
				// they can be removed later.
				tagVals.RangeAll(func(_ string, a SeriesIDs) {
					seriesIDs.mark(a)
				})

				// Make a new slice with only the remaining ids.
				ids = seriesIDs.evict()
			}
		} else if n.Op == influxql.NEQ {
			if str.Val != "" {
				ids = m.SeriesIDs().Reject(tagVals.Load(str.Val))
			} else {
				tagVals.RangeAll(func(_ string, a SeriesIDs) {
					ids = append(ids, a...)
				})
				sort.Sort(ids)
			}
		}
		return ids, nil, nil
	}

	// if we're looking for series with a tag value that matches a regex
	if re, ok := value.(*influxql.RegexLiteral); ok {
		var ids SeriesIDs

		// Special handling for "_name" to match measurement name.
		if name.Val == "_name" {
			match := re.Val.MatchString(m.Name)
			if (n.Op == influxql.EQREGEX && match) || (n.Op == influxql.NEQREGEX && !match) {
				return m.SeriesIDs(), &influxql.BooleanLiteral{Val: true}, nil
			}
			return nil, nil, nil
		}

		// Check if we match the empty string to see if we should include series
		// that are missing the tag.
		empty := re.Val.MatchString("")

		// Gather the series that match the regex. If we should include the empty string,
		// start with the list of all series and reject series that don't match our condition.
		// If we should not include the empty string, include series that match our condition.
		if empty && n.Op == influxql.EQREGEX {
			// See comments above for EQ with a StringLiteral.
			seriesIDs := newEvictSeriesIDs(m.SeriesIDs())
			tagVals.RangeAll(func(k string, a SeriesIDs) {
				if !re.Val.MatchString(k) {
					seriesIDs.mark(a)
				}
			})
			ids = seriesIDs.evict()
		} else if empty && n.Op == influxql.NEQREGEX {
			ids = make(SeriesIDs, 0, len(m.SeriesIDs()))
			tagVals.RangeAll(func(k string, a SeriesIDs) {
				if !re.Val.MatchString(k) {
					ids = append(ids, a...)
				}
			})
			sort.Sort(ids)
		} else if !empty && n.Op == influxql.EQREGEX {
			ids = make(SeriesIDs, 0, len(m.SeriesIDs()))
			tagVals.RangeAll(func(k string, a SeriesIDs) {
				if re.Val.MatchString(k) {
					ids = append(ids, a...)
				}
			})
			sort.Sort(ids)
		} else if !empty && n.Op == influxql.NEQREGEX {
			// See comments above for EQ with a StringLiteral.
			seriesIDs := newEvictSeriesIDs(m.SeriesIDs())
			tagVals.RangeAll(func(k string, a SeriesIDs) {
				if re.Val.MatchString(k) {
					seriesIDs.mark(a)
				}
			})
			ids = seriesIDs.evict()
		}
		return ids, nil, nil
	}

	// compare tag values
	if ref, ok := value.(*influxql.VarRef); ok {
		var ids SeriesIDs

		if n.Op == influxql.NEQ {
			ids = m.SeriesIDs()
		}

		rhsTagVals := m.seriesByTagKeyValue[ref.Val]
		tagVals.RangeAll(func(k string, a SeriesIDs) {
			tags := a.Intersect(rhsTagVals.Load(k))
			if n.Op == influxql.EQ {
				ids = ids.Union(tags)
			} else if n.Op == influxql.NEQ {
				ids = ids.Reject(tags)
			}
		})
		return ids, nil, nil
	}

	if n.Op == influxql.NEQ || n.Op == influxql.NEQREGEX {
		return m.SeriesIDs(), nil, nil
	}
	return nil, nil, nil
}

// FilterExprs represents a map of series IDs to filter expressions.
type FilterExprs map[uint64]influxql.Expr

// DeleteBoolLiteralTrues deletes all elements whose filter expression is a boolean literal true.
func (fe FilterExprs) DeleteBoolLiteralTrues() {
	for id, expr := range fe {
		if e, ok := expr.(*influxql.BooleanLiteral); ok && e.Val {
			delete(fe, id)
		}
	}
}

// Len returns the number of elements.
func (fe FilterExprs) Len() int {
	if fe == nil {
		return 0
	}
	return len(fe)
}

// WalkWhereForSeriesIds recursively walks the WHERE clause and returns an ordered set of series IDs and
// a map from those series IDs to filter expressions that should be used to limit points returned in
// the final query result.
func (m *Measurement) WalkWhereForSeriesIds(expr influxql.Expr) (SeriesIDs, FilterExprs, error) {
	switch n := expr.(type) {
	case *influxql.BinaryExpr:
		switch n.Op {
		case influxql.EQ, influxql.NEQ, influxql.LT, influxql.LTE, influxql.GT, influxql.GTE, influxql.EQREGEX, influxql.NEQREGEX:
			// Get the series IDs and filter expression for the tag or field comparison.
			ids, expr, err := m.idsForExpr(n)
			if err != nil {
				return nil, nil, err
			}

			if len(ids) == 0 {
				return ids, nil, nil
			}

			// If the expression is a boolean literal that is true, ignore it.
			if b, ok := expr.(*influxql.BooleanLiteral); ok && b.Val {
				expr = nil
			}

			var filters FilterExprs
			if expr != nil {
				filters = make(FilterExprs, len(ids))
				for _, id := range ids {
					filters[id] = expr
				}
			}

			return ids, filters, nil
		case influxql.AND, influxql.OR:
			// Get the series IDs and filter expressions for the LHS.
			lids, lfilters, err := m.WalkWhereForSeriesIds(n.LHS)
			if err != nil {
				return nil, nil, err
			}

			// Get the series IDs and filter expressions for the RHS.
			rids, rfilters, err := m.WalkWhereForSeriesIds(n.RHS)
			if err != nil {
				return nil, nil, err
			}

			// Combine the series IDs from the LHS and RHS.
			if n.Op == influxql.AND {
				ids, filters := intersectSeriesFilters(lids, rids, lfilters, rfilters)
				return ids, filters, nil
			} else {
				ids, filters := unionSeriesFilters(lids, rids, lfilters, rfilters)
				return ids, filters, nil
			}
		}

		ids, _, err := m.idsForExpr(n)
		return ids, nil, err
	case *influxql.ParenExpr:
		// walk down the tree
		return m.WalkWhereForSeriesIds(n.Expr)
	default:
		return nil, nil, nil
	}
}

// expandExpr returns a list of expressions expanded by all possible tag
// combinations.
func (m *Measurement) expandExpr(expr influxql.Expr) []tagSetExpr {
	// Retrieve list of unique values for each tag.
	valuesByTagKey := m.uniqueTagValues(expr)

	// Convert keys to slices.
	keys := make([]string, 0, len(valuesByTagKey))
	for key := range valuesByTagKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// Order uniques by key.
	uniques := make([][]string, len(keys))
	for i, key := range keys {
		uniques[i] = valuesByTagKey[key]
	}

	// Reduce a condition for each combination of tag values.
	return expandExprWithValues(expr, keys, []tagExpr{}, uniques, 0)
}

func expandExprWithValues(expr influxql.Expr, keys []string, tagExprs []tagExpr, uniques [][]string, index int) []tagSetExpr {
	// If we have no more keys left then execute the reduction and return.
	if index == len(keys) {
		// Create a map of tag key/values.
		m := make(map[string]*string, len(keys))
		for i, key := range keys {
			if tagExprs[i].op == influxql.EQ {
				m[key] = &tagExprs[i].values[0]
			} else {
				m[key] = nil
			}
		}

		// TODO: Rewrite full expressions instead of VarRef replacement.

		// Reduce using the current tag key/value set.
		// Ignore it if reduces down to "false".
		e := influxql.Reduce(expr, &tagValuer{tags: m})
		if e, ok := e.(*influxql.BooleanLiteral); ok && !e.Val {
			return nil
		}

		return []tagSetExpr{{values: copyTagExprs(tagExprs), expr: e}}
	}

	// Otherwise expand for each possible equality value of the key.
	var exprs []tagSetExpr
	for _, v := range uniques[index] {
		exprs = append(exprs, expandExprWithValues(expr, keys, append(tagExprs, tagExpr{keys[index], []string{v}, influxql.EQ}), uniques, index+1)...)
	}
	exprs = append(exprs, expandExprWithValues(expr, keys, append(tagExprs, tagExpr{keys[index], uniques[index], influxql.NEQ}), uniques, index+1)...)

	return exprs
}

// SeriesIDsAllOrByExpr walks an expressions for matching series IDs
// or, if no expressions is given, returns all series IDs for the measurement.
func (m *Measurement) SeriesIDsAllOrByExpr(expr influxql.Expr) (SeriesIDs, error) {
	// If no expression given or the measurement has no series,
	// we can take just return the ids or nil accordingly.
	if expr == nil {
		return m.SeriesIDs(), nil
	}

	m.mu.RLock()
	l := len(m.seriesByID)
	m.mu.RUnlock()
	if l == 0 {
		return nil, nil
	}

	// Get series IDs that match the WHERE clause.
	ids, _, err := m.WalkWhereForSeriesIds(expr)
	if err != nil {
		return nil, err
	}

	return ids, nil
}

// tagKeysByExpr extracts the tag keys wanted by the expression.
func (m *Measurement) TagKeysByExpr(expr influxql.Expr) (map[string]struct{}, error) {
	if expr == nil {
		set := make(map[string]struct{})
		for _, key := range m.TagKeys() {
			set[key] = struct{}{}
		}
		return set, nil
	}

	switch e := expr.(type) {
	case *influxql.BinaryExpr:
		switch e.Op {
		case influxql.EQ, influxql.NEQ, influxql.EQREGEX, influxql.NEQREGEX:
			tag, ok := e.LHS.(*influxql.VarRef)
			if !ok {
				return nil, fmt.Errorf("left side of '%s' must be a tag key", e.Op.String())
			} else if tag.Val != "_tagKey" {
				return nil, nil
			}

			if influxql.IsRegexOp(e.Op) {
				re, ok := e.RHS.(*influxql.RegexLiteral)
				if !ok {
					return nil, fmt.Errorf("right side of '%s' must be a regular expression", e.Op.String())
				}
				return m.tagKeysByFilter(e.Op, "", re.Val), nil
			}

			s, ok := e.RHS.(*influxql.StringLiteral)
			if !ok {
				return nil, fmt.Errorf("right side of '%s' must be a tag value string", e.Op.String())
			}
			return m.tagKeysByFilter(e.Op, s.Val, nil), nil

		case influxql.AND, influxql.OR:
			lhs, err := m.TagKeysByExpr(e.LHS)
			if err != nil {
				return nil, err
			}

			rhs, err := m.TagKeysByExpr(e.RHS)
			if err != nil {
				return nil, err
			}

			if lhs != nil && rhs != nil {
				if e.Op == influxql.OR {
					return stringSet(lhs).union(rhs), nil
				}
				return stringSet(lhs).intersect(rhs), nil
			} else if lhs != nil {
				return lhs, nil
			} else if rhs != nil {
				return rhs, nil
			}
			return nil, nil
		default:
			return nil, fmt.Errorf("invalid operator")
		}

	case *influxql.ParenExpr:
		return m.TagKeysByExpr(e.Expr)
	}

	return nil, fmt.Errorf("%#v", expr)
}

// tagKeysByFilter will filter the tag keys for the measurement.
func (m *Measurement) tagKeysByFilter(op influxql.Token, val string, regex *regexp.Regexp) stringSet {
	ss := newStringSet()
	for _, key := range m.TagKeys() {
		var matched bool
		switch op {
		case influxql.EQ:
			matched = key == val
		case influxql.NEQ:
			matched = key != val
		case influxql.EQREGEX:
			matched = regex.MatchString(key)
		case influxql.NEQREGEX:
			matched = !regex.MatchString(key)
		}

		if !matched {
			continue
		}
		ss.add(key)
	}
	return ss
}

// tagValuer is used during expression expansion to evaluate all sets of tag values.
type tagValuer struct {
	tags map[string]*string
}

// Value returns the string value of a tag and true if it's listed in the tagset.
func (v *tagValuer) Value(name string) (interface{}, bool) {
	if value, ok := v.tags[name]; ok {
		if value == nil {
			return nil, true
		}
		return *value, true
	}
	return nil, false
}

// tagSetExpr represents a set of tag keys/values and associated expression.
type tagSetExpr struct {
	values []tagExpr
	expr   influxql.Expr
}

// tagExpr represents one or more values assigned to a given tag.
type tagExpr struct {
	key    string
	values []string
	op     influxql.Token // EQ or NEQ
}

func copyTagExprs(a []tagExpr) []tagExpr {
	other := make([]tagExpr, len(a))
	copy(other, a)
	return other
}

// uniqueTagValues returns a list of unique tag values used in an expression.
func (m *Measurement) uniqueTagValues(expr influxql.Expr) map[string][]string {
	// Track unique value per tag.
	tags := make(map[string]map[string]struct{})

	// Find all tag values referenced in the expression.
	influxql.WalkFunc(expr, func(n influxql.Node) {
		switch n := n.(type) {
		case *influxql.BinaryExpr:
			// Ignore operators that are not equality.
			if n.Op != influxql.EQ {
				return
			}

			// Extract ref and string literal.
			var key, value string
			switch lhs := n.LHS.(type) {
			case *influxql.VarRef:
				if rhs, ok := n.RHS.(*influxql.StringLiteral); ok {
					key, value = lhs.Val, rhs.Val
				}
			case *influxql.StringLiteral:
				if rhs, ok := n.RHS.(*influxql.VarRef); ok {
					key, value = rhs.Val, lhs.Val
				}
			}
			if key == "" {
				return
			}

			// Add value to set.
			if tags[key] == nil {
				tags[key] = make(map[string]struct{})
			}
			tags[key][value] = struct{}{}
		}
	})

	// Convert to map of slices.
	out := make(map[string][]string)
	for k, values := range tags {
		out[k] = make([]string, 0, len(values))
		for v := range values {
			out[k] = append(out[k], v)
		}
		sort.Strings(out[k])
	}
	return out
}

// Measurements represents a list of *Measurement.
type Measurements []*Measurement

// Len implements sort.Interface.
func (a Measurements) Len() int { return len(a) }

// Less implements sort.Interface.
func (a Measurements) Less(i, j int) bool { return a[i].Name < a[j].Name }

// Swap implements sort.Interface.
func (a Measurements) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

func (a Measurements) Intersect(other Measurements) Measurements {
	l := a
	r := other

	// we want to iterate through the shortest one and stop
	if len(other) < len(a) {
		l = other
		r = a
	}

	// they're in sorted order so advance the counter as needed.
	// That is, don't run comparisons against lower values that we've already passed
	var i, j int

	result := make(Measurements, 0, len(l))
	for i < len(l) && j < len(r) {
		if l[i].Name == r[j].Name {
			result = append(result, l[i])
			i++
			j++
		} else if l[i].Name < r[j].Name {
			i++
		} else {
			j++
		}
	}

	return result
}

func (a Measurements) Union(other Measurements) Measurements {
	result := make(Measurements, 0, len(a)+len(other))
	var i, j int
	for i < len(a) && j < len(other) {
		if a[i].Name == other[j].Name {
			result = append(result, a[i])
			i++
			j++
		} else if a[i].Name < other[j].Name {
			result = append(result, a[i])
			i++
		} else {
			result = append(result, other[j])
			j++
		}
	}

	// now append the remainder
	if i < len(a) {
		result = append(result, a[i:]...)
	} else if j < len(other) {
		result = append(result, other[j:]...)
	}

	return result
}

// Series belong to a Measurement and represent unique time series in a database.
type Series struct {
	mu          sync.RWMutex
	Key         string
	tags        models.Tags
	ID          uint64
	measurement *Measurement
	shardIDs    map[uint64]struct{} // shards that have this series defined
	deleted     bool

	// lastModified tracks the last time the series was created.  If the series
	// already exists and a request to create is received (a no-op), lastModified
	// is increased to track that it is still in use.
	lastModified int64
}

// NewSeries returns an initialized series struct
func NewSeries(key []byte, tags models.Tags) *Series {
	return &Series{
		Key:          string(key),
		tags:         tags,
		shardIDs:     make(map[uint64]struct{}),
		lastModified: time.Now().UTC().UnixNano(),
	}
}

func (s *Series) AssignShard(shardID uint64) {
	atomic.StoreInt64(&s.lastModified, time.Now().UTC().UnixNano())
	if s.Assigned(shardID) {
		return
	}

	s.mu.Lock()
	// Skip the existence check under the write lock because we're just storing
	// and empty struct.
	s.deleted = false
	s.shardIDs[shardID] = struct{}{}
	s.mu.Unlock()
}

func (s *Series) UnassignShard(shardID uint64, ts int64) {
	s.mu.Lock()
	if s.LastModified() < ts {
		delete(s.shardIDs, shardID)
	}
	s.mu.Unlock()
}

func (s *Series) Assigned(shardID uint64) bool {
	s.mu.RLock()
	_, ok := s.shardIDs[shardID]
	s.mu.RUnlock()
	return ok
}

func (s *Series) LastModified() int64 {
	return atomic.LoadInt64(&s.lastModified)
}

func (s *Series) ShardN() int {
	s.mu.RLock()
	n := len(s.shardIDs)
	s.mu.RUnlock()
	return n
}

// Measurement returns the measurement on the series.
func (s *Series) Measurement() *Measurement {
	return s.measurement
}

// SetMeasurement sets the measurement on the series.
func (s *Series) SetMeasurement(m *Measurement) {
	s.measurement = m
}

// ForEachTag executes fn for every tag. Iteration occurs under lock.
func (s *Series) ForEachTag(fn func(models.Tag)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tags {
		fn(t)
	}
}

// Tags returns a copy of the tags under lock.
func (s *Series) Tags() models.Tags {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tags
}

// CopyTags clones the tags on the series in-place,
func (s *Series) CopyTags() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tags = s.tags.Clone()
}

// GetTagString returns a tag value under lock.
func (s *Series) GetTagString(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tags.GetString(key)
}

// Delete marks this series as deleted.  A deleted series should not be returned for queries.
func (s *Series) Delete(ts int64) {
	s.mu.Lock()
	if s.LastModified() < ts {
		s.deleted = true
	}
	s.mu.Unlock()
}

// Deleted indicates if this was previously deleted.
func (s *Series) Deleted() bool {
	s.mu.RLock()
	v := s.deleted
	s.mu.RUnlock()
	return v
}

// TagKeyValue provides goroutine-safe concurrent access to the set of series
// ids mapping to a set of tag values.
//
// TODO(edd): This could possibly be replaced by a sync.Map once we use Go 1.9.
type TagKeyValue struct {
	mu       sync.RWMutex
	valueIDs map[string]SeriesIDs
}

// NewTagKeyValue initialises a new TagKeyValue.
func NewTagKeyValue() *TagKeyValue {
	return &TagKeyValue{valueIDs: make(map[string]SeriesIDs)}
}

// Cardinality returns the number of values in the TagKeyValue.
func (t *TagKeyValue) Cardinality() int {
	if t == nil {
		return 0
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.valueIDs)
}

// Contains returns true if the TagKeyValue contains value.
func (t *TagKeyValue) Contains(value string) bool {
	if t == nil {
		return false
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.valueIDs[value]
	return ok
}

// Load returns the SeriesIDs for the provided tag value.
func (t *TagKeyValue) Load(value string) SeriesIDs {
	if t == nil {
		return nil
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.valueIDs[value]
}

// LoadByte returns the SeriesIDs for the provided tag value. It makes use of
// Go's compiler optimisation for avoiding a copy when accessing maps with a []byte.
func (t *TagKeyValue) LoadByte(value []byte) SeriesIDs {
	if t == nil {
		return nil
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.valueIDs[string(value)]
}

// Range calls f sequentially on each key and value. A call to Range on a nil
// TagKeyValue is a no-op.
//
// If f returns false then iteration over any remaining keys or values will cease.
func (t *TagKeyValue) Range(f func(k string, a SeriesIDs) bool) {
	if t == nil {
		return
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	for k, a := range t.valueIDs {
		if !f(k, a) {
			return
		}
	}
}

// RangeAll calls f sequentially on each key and value. A call to RangeAll on a
// nil TagKeyValue is a no-op.
func (t *TagKeyValue) RangeAll(f func(k string, a SeriesIDs)) {
	t.Range(func(k string, a SeriesIDs) bool {
		f(k, a)
		return true
	})
}

// Store stores ids under the value key.
func (t *TagKeyValue) Store(value string, ids SeriesIDs) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.valueIDs[value] = ids
}

// SeriesIDs is a convenience type for sorting, checking equality, and doing
// union and intersection of collections of series ids.
type SeriesIDs []uint64

// Len implements sort.Interface.
func (a SeriesIDs) Len() int { return len(a) }

// Less implements sort.Interface.
func (a SeriesIDs) Less(i, j int) bool { return a[i] < a[j] }

// Swap implements sort.Interface.
func (a SeriesIDs) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

// Equals assumes that both are sorted.
func (a SeriesIDs) Equals(other SeriesIDs) bool {
	if len(a) != len(other) {
		return false
	}
	for i, s := range other {
		if a[i] != s {
			return false
		}
	}
	return true
}

// Intersect returns a new collection of series ids in sorted order that is the intersection of the two.
// The two collections must already be sorted.
func (a SeriesIDs) Intersect(other SeriesIDs) SeriesIDs {
	l := a
	r := other

	// we want to iterate through the shortest one and stop
	if len(other) < len(a) {
		l = other
		r = a
	}

	// they're in sorted order so advance the counter as needed.
	// That is, don't run comparisons against lower values that we've already passed
	var i, j int

	ids := make([]uint64, 0, len(l))
	for i < len(l) && j < len(r) {
		if l[i] == r[j] {
			ids = append(ids, l[i])
			i++
			j++
		} else if l[i] < r[j] {
			i++
		} else {
			j++
		}
	}

	return SeriesIDs(ids)
}

// Union returns a new collection of series ids in sorted order that is the union of the two.
// The two collections must already be sorted.
func (a SeriesIDs) Union(other SeriesIDs) SeriesIDs {
	l := a
	r := other
	ids := make([]uint64, 0, len(l)+len(r))
	var i, j int
	for i < len(l) && j < len(r) {
		if l[i] == r[j] {
			ids = append(ids, l[i])
			i++
			j++
		} else if l[i] < r[j] {
			ids = append(ids, l[i])
			i++
		} else {
			ids = append(ids, r[j])
			j++
		}
	}

	// now append the remainder
	if i < len(l) {
		ids = append(ids, l[i:]...)
	} else if j < len(r) {
		ids = append(ids, r[j:]...)
	}

	return ids
}

// Reject returns a new collection of series ids in sorted order with the passed in set removed from the original.
// This is useful for the NOT operator. The two collections must already be sorted.
func (a SeriesIDs) Reject(other SeriesIDs) SeriesIDs {
	l := a
	r := other
	var i, j int

	ids := make([]uint64, 0, len(l))
	for i < len(l) && j < len(r) {
		if l[i] == r[j] {
			i++
			j++
		} else if l[i] < r[j] {
			ids = append(ids, l[i])
			i++
		} else {
			j++
		}
	}

	// Append the remainder
	if i < len(l) {
		ids = append(ids, l[i:]...)
	}

	return SeriesIDs(ids)
}

// seriesID is a series id that may or may not have been evicted from the
// current id list.
type seriesID struct {
	val   uint64
	evict bool
}

// evictSeriesIDs is a slice of SeriesIDs with an extra field to mark if the
// field should be evicted or not.
type evictSeriesIDs struct {
	ids []seriesID
	sz  int
}

// newEvictSeriesIDs copies the ids into a new slice that can be used for
// evicting series from the slice.
func newEvictSeriesIDs(ids []uint64) evictSeriesIDs {
	a := make([]seriesID, len(ids))
	for i, id := range ids {
		a[i].val = id
	}
	return evictSeriesIDs{
		ids: a,
		sz:  len(a),
	}
}

// mark marks all of the ids in the sorted slice to be evicted from the list of
// series ids. If an id to be evicted does not exist, it just gets ignored.
func (a *evictSeriesIDs) mark(ids []uint64) {
	seriesIDs := a.ids
	for _, id := range ids {
		if len(seriesIDs) == 0 {
			break
		}

		// Perform a binary search of the remaining slice if
		// the first element does not match the value we're
		// looking for.
		i := 0
		if seriesIDs[0].val < id {
			i = sort.Search(len(seriesIDs), func(i int) bool {
				return seriesIDs[i].val >= id
			})
		}

		if i >= len(seriesIDs) {
			break
		} else if seriesIDs[i].val == id {
			if !seriesIDs[i].evict {
				seriesIDs[i].evict = true
				a.sz--
			}
			// Skip over this series since it has been evicted and won't be
			// encountered again.
			i++
		}
		seriesIDs = seriesIDs[i:]
	}
}

// evict creates a new slice with only the series that have not been evicted.
func (a *evictSeriesIDs) evict() (ids SeriesIDs) {
	if a.sz == 0 {
		return ids
	}

	// Make a new slice with only the remaining ids.
	ids = make([]uint64, 0, a.sz)
	for _, id := range a.ids {
		if id.evict {
			continue
		}
		ids = append(ids, id.val)
	}
	return ids
}

// TagFilter represents a tag filter when looking up other tags or measurements.
type TagFilter struct {
	Op    influxql.Token
	Key   string
	Value string
	Regex *regexp.Regexp
}

// WalkTagKeys calls fn for each tag key associated with m.  The order of the
// keys is undefined.
func (m *Measurement) WalkTagKeys(fn func(k string)) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for k := range m.seriesByTagKeyValue {
		fn(k)
	}
}

// TagKeys returns a list of the measurement's tag names, in sorted order.
func (m *Measurement) TagKeys() []string {
	m.mu.RLock()
	keys := make([]string, 0, len(m.seriesByTagKeyValue))
	for k := range m.seriesByTagKeyValue {
		keys = append(keys, k)
	}
	m.mu.RUnlock()
	sort.Strings(keys)
	return keys
}

// TagValues returns all the values for the given tag key, in an arbitrary order.
func (m *Measurement) TagValues(auth query.Authorizer, key string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	values := make([]string, 0, m.seriesByTagKeyValue[key].Cardinality())

	m.seriesByTagKeyValue[key].RangeAll(func(k string, a SeriesIDs) {
		if auth == nil {
			values = append(values, k)
		} else {
			for _, sid := range a {
				s := m.seriesByID[sid]
				if s == nil {
					continue
				}
				if auth.AuthorizeSeriesRead(m.database, m.name, s.Tags()) {
					values = append(values, k)
					return
				}
			}
		}
	})
	return values
}

// SetFieldName adds the field name to the measurement.
func (m *Measurement) SetFieldName(name string) {
	m.mu.RLock()
	_, ok := m.fieldNames[name]
	m.mu.RUnlock()

	if ok {
		return
	}

	m.mu.Lock()
	m.fieldNames[name] = struct{}{}
	m.mu.Unlock()
}

// FieldNames returns a list of the measurement's field names, in an arbitrary order.
func (m *Measurement) FieldNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	a := make([]string, 0, len(m.fieldNames))
	for n := range m.fieldNames {
		a = append(a, n)
	}
	return a
}

// SeriesByTagKeyValue returns the TagKeyValue for the provided tag key.
func (m *Measurement) SeriesByTagKeyValue(key string) *TagKeyValue {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.seriesByTagKeyValue[key]
}

// stringSet represents a set of strings.
type stringSet map[string]struct{}

// newStringSet returns an empty stringSet.
func newStringSet() stringSet {
	return make(map[string]struct{})
}

// add adds strings to the set.
func (s stringSet) add(ss ...string) {
	for _, n := range ss {
		s[n] = struct{}{}
	}
}

// list returns the current elements in the set, in sorted order.
func (s stringSet) list() []string {
	l := make([]string, 0, len(s))
	for k := range s {
		l = append(l, k)
	}
	sort.Strings(l)
	return l
}

// union returns the union of this set and another.
func (s stringSet) union(o stringSet) stringSet {
	ns := newStringSet()
	for k := range s {
		ns[k] = struct{}{}
	}
	for k := range o {
		ns[k] = struct{}{}
	}
	return ns
}

// intersect returns the intersection of this set and another.
func (s stringSet) intersect(o stringSet) stringSet {
	shorter, longer := s, o
	if len(longer) < len(shorter) {
		shorter, longer = longer, shorter
	}

	ns := newStringSet()
	for k := range shorter {
		if _, ok := longer[k]; ok {
			ns[k] = struct{}{}
		}
	}
	return ns
}

// filter removes v from a if it exists.  a must be sorted in ascending
// order.
func filter(a []uint64, v uint64) []uint64 {
	// binary search for v
	i := sort.Search(len(a), func(i int) bool { return a[i] >= v })
	if i >= len(a) || a[i] != v {
		return a
	}

	// we found it, so shift the right half down one, overwriting v's position.
	copy(a[i:], a[i+1:])
	return a[:len(a)-1]
}

type byTagKey []*query.TagSet

func (t byTagKey) Len() int           { return len(t) }
func (t byTagKey) Less(i, j int) bool { return bytes.Compare(t[i].Key, t[j].Key) < 0 }
func (t byTagKey) Swap(i, j int)      { t[i], t[j] = t[j], t[i] }
