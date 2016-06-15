package main

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// A ClientSideIndex wraps runtime data used to translate an HLQuery into
// Cassandra CQL queries. After initialization, objects of this type are
// read-only.
type ClientSideIndex struct {
	timeIntervalMapping map[TimeInterval]map[*Series]struct{}
	tagMapping          map[string]map[*Series]struct{}

	seriesCollection []Series
	seriesIds        []string
}

// NewClientSideIndex constructs a ClientSideIndex from a precomputed
// seriesCollection (typically by calling FetchSeriesCollection).
func NewClientSideIndex(seriesCollection []Series) *ClientSideIndex {
	if len(seriesCollection) == 0 {
		log.Fatal("logic error: no data to build ClientSideIndex")
	}

	// build the "time interval -> series" index:
	bm := map[TimeInterval]map[*Series]struct{}{}

	for _, s := range seriesCollection {
		if _, ok := bm[s.TimeInterval]; !ok {
			bm[s.TimeInterval] = map[*Series]struct{}{}
		}
		if _, ok := bm[s.TimeInterval][&s]; !ok {
			bm[s.TimeInterval][&s] = struct{}{}
		}
	}

	// build the "tag -> series" index:
	tm := map[string]map[*Series]struct{}{}

	for _, s := range seriesCollection {
		for tag, _ := range s.Tags {
			if _, ok := tm[tag]; !ok {
				tm[tag] = map[*Series]struct{}{}
			}
			if _, ok := tm[tag][&s]; !ok {
				tm[tag][&s] = struct{}{}
			}
		}
	}

	// build the series id collection:
	seriesIds := make([]string, 0, len(seriesCollection))
	for _, s := range seriesCollection {
		seriesIds = append(seriesIds, s.Id)
	}

	return &ClientSideIndex{
		timeIntervalMapping: bm,
		tagMapping:          tm,
		seriesCollection:    seriesCollection,
		seriesIds:           seriesIds,
	}
}

// CopyOfSeriesCollection returns a copy of the internal Series data. It's
// output slice can be safely altered, but the Series objects within may not!
func (csi *ClientSideIndex) CopyOfSeriesCollection() []Series {
	ret := make([]Series, len(csi.seriesCollection))
	copy(ret, csi.seriesCollection)
	return ret
}

// A Series maps 1-to-1 to a time series 'wide row' in Cassandra. All data in
// this type comes directly from a Cassandra database.
type Series struct {
	Table string // e.g. "series_bigint"
	Id    string // e.g. "cpu,hostname=host_0,region=eu-central-1#usage_idle#2016-01-01"

	// parsed fields
	Measurement  string              // e.g. "cpu"
	Tags         map[string]struct{} // e.g. {"hostname": "host_3"}
	Field        string              // e.g. "usage_idle"
	TimeInterval TimeInterval        // (UTC) e.g. "2016-01-01"
}

// NewSeries parses a new Series from the given Cassandra data.
func NewSeries(table, id string) Series {
	s := Series{
		Table: table,
		Id:    id,
	}

	s.parse()
	return s
}

func (s *Series) parse() {
	// expected format:
	// cpu,hostname=host_0,region=eu-central-1,datacenter=eu-central-1a,rack=42,os=Ubuntu16.10,arch=x64,team=CHI,service=19,service_version=1,service_environment=staging#usage_idle#2016-01-01
	sections := strings.Split(s.Id, "#")
	if len(sections) != 3 {
		log.Fatal("logic error: invalid series id")
	}
	measurementAndTags := strings.Split(sections[0], ",")

	// parse measurement:
	s.Measurement = measurementAndTags[0]

	// parse tags:
	tags := map[string]struct{}{}
	for _, tag := range measurementAndTags[1:] {
		if _, ok := tags[tag]; ok {
			log.Fatal("logic error: duplicate tag")
		}

		tags[tag] = struct{}{}
	}
	s.Tags = tags

	// parse field name:
	s.Field = sections[1]

	// parse time interval:
	start, err := time.Parse("2006-01-02", sections[2])
	if err != nil {
		log.Fatal("bad time bucket parse in pre-existing database series")
	}
	end := start.Add(BucketDuration)
	s.TimeInterval = NewTimeInterval(start, end)
}

// MatchesTimeInterval determines whether this Series time overlaps with the
// provided TimeInterval.
func (s *Series) MatchesTimeInterval(ti *TimeInterval) bool {
	return s.TimeInterval.Overlap(ti)
}

// MatchesMeasurementName determines whether this Series measurement name matches
// the provided name.
func (s *Series) MatchesMeasurementName(m string) bool {
	return s.Measurement == m
}

// MatchesFieldName determines whether this Series field name matches
// the provided name.
func (s *Series) MatchesFieldName(f string) bool {
	return s.Field == f
}

// MatchesTagSets checks whether this Series matches the given tagsets.
func (s *Series) MatchesTagSets(tagsets [][]string) bool {
	for _, tagset := range tagsets {
		// each tagset must have at least one match
		match := false
		for _, tag := range tagset {
			if _, ok := s.Tags[tag]; ok {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}

// FetchSeriesCollection returns all series in Cassandra that can be used for
// fulfilling a query.
func FetchSeriesCollection(daemonUrl string) []Series {
	session := NewCassandraSession(daemonUrl)
	defer session.Close()

	seriesCollection := []Series{}

	for _, tableName := range BlessedTables {
		var seriesId string
		iter := session.Query(fmt.Sprintf(`SELECT DISTINCT series_id FROM %s`, tableName)).Iter()
		for iter.Scan(&seriesId) {
			s := NewSeries(tableName, seriesId)
			seriesCollection = append(seriesCollection, s)
		}
		if err := iter.Close(); err != nil {
			log.Fatal(err)
		}
	}
	return seriesCollection
}
