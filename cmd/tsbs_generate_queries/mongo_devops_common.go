package main

import (
	"encoding/gob"
	"fmt"
	"time"

	"bitbucket.org/440-labs/influxdb-comparisons/query"
	"github.com/globalsign/mgo/bson"
)

func init() {
	// needed for serializing the mongo query to gob
	gob.Register([]interface{}{})
	gob.Register(map[string]interface{}{})
	gob.Register([]map[string]interface{}{})
	gob.Register(bson.M{})
	gob.Register([]bson.M{})
}

// MongoDevops produces Mongo-specific queries for the devops use case.
type MongoDevops struct {
	*devopsCore
}

// NewMongoDevops makes an MongoDevops object ready to generate Queries.
func newMongoDevopsCommon(start, end time.Time, scale int) *MongoDevops {
	return &MongoDevops{newDevopsCore(start, end, scale)}
}

// GenerateEmptyQuery returns an empty query.Mongo
func (d *MongoDevops) GenerateEmptyQuery() query.Query {
	return query.NewMongo()
}

func getTimeFilterPipeline(interval TimeInterval) []bson.M {
	return []bson.M{
		{"$unwind": "$events"},
		{
			"$project": bson.M{
				"key_id": 1,
				"tags":   1,
				"events": bson.M{
					"$filter": bson.M{
						"input": "$events",
						"as":    "event",
						"cond": bson.M{
							"$and": []interface{}{
								bson.M{
									"$gte": []interface{}{
										"$$event.timestamp_ns",
										interval.StartUnixNano(),
									},
								},
								bson.M{
									"$lt": []interface{}{
										"$$event.timestamp_ns",
										interval.EndUnixNano(),
									},
								},
							},
						},
					},
				},
			},
		},
		{"$unwind": "$events"},
	}
}

const aggDateFmt = "20060102" // see Go docs for how we arrive at this time format

func getTimeFilterDocs(interval TimeInterval) []interface{} {
	docs := []interface{}{}
	startDay := interval.Start.Format(aggDateFmt)
	startHr := interval.Start.Hour()
	lenHrs := int(interval.Duration()/time.Hour) + 1
	for i := 0; i < lenHrs; i++ {
		hr := int(startHr) + i
		if hr > 23 {
			days := int64(hr / 24)
			day := interval.Start.Add(time.Duration(days * 24 * 60 * 60 * 1e9))
			docs = append(docs, fmt.Sprintf("%s_%02d", day.Format(aggDateFmt), hr%24))
		} else {
			docs = append(docs, fmt.Sprintf("%s_%02d", startDay, hr))
		}
	}

	return docs
}

// GroupByTime selects the MAX for numMetrics metrics under 'cpu',
// per minute for nhosts hosts,
// e.g. in psuedo-SQL:
//
// SELECT minute, max(metric1), ..., max(metricN)
// FROM cpu
// WHERE (hostname = '$HOSTNAME_1' OR ... OR hostname = '$HOSTNAME_N')
// AND time >= '$HOUR_START' AND time < '$HOUR_END'
// GROUP BY minute ORDER BY minute ASC
func (d *MongoDevops) GroupByTime(qi query.Query, nHosts, numMetrics int, timeRange time.Duration) {
	interval := d.interval.RandWindow(timeRange)
	hostnames := d.getRandomHosts(nHosts)
	metrics := getCPUMetricsSlice(numMetrics)
	docs := getTimeFilterDocs(interval)
	bucketNano := time.Minute.Nanoseconds()

	pipelineQuery := []bson.M{
		{
			"$match": bson.M{
				"measurement": "cpu",
				"tags.hostname": bson.M{
					"$in": hostnames,
				},
				"key_id": bson.M{
					"$in": docs,
				},
			},
		},
		{
			"$project": bson.M{
				"_id":    0,
				"events": 1,
				"key_id": 1,
				"tags":   "$tags.hostname",
			},
		},
	}
	pipelineQuery = append(pipelineQuery, getTimeFilterPipeline(interval)...)
	pipelineQuery = append(pipelineQuery, bson.M{
		"$project": bson.M{
			"time_bucket": bson.M{
				"$subtract": []interface{}{
					"$events.timestamp_ns",
					bson.M{"$mod": []interface{}{"$events.timestamp_ns", bucketNano}},
				},
			},
			"events": 1,
		},
	})

	group := bson.M{
		"$group": bson.M{
			"_id": "$time_bucket",
		},
	}
	resultMap := group["$group"].(bson.M)
	for _, metric := range metrics {
		resultMap["max_"+metric] = bson.M{"$max": "$events." + metric}
	}
	pipelineQuery = append(pipelineQuery, group)
	pipelineQuery = append(pipelineQuery, bson.M{"$sort": bson.M{"_id": 1}})

	humanLabel := []byte(fmt.Sprintf("Mongo %d cpu metric(s), random %4d hosts, random %s by 1m", numMetrics, nHosts, timeRange))
	q := qi.(*query.Mongo)
	q.HumanLabel = humanLabel
	q.BsonDoc = pipelineQuery
	q.CollectionName = []byte("point_data")
	q.HumanDescription = []byte(fmt.Sprintf("%s: %s (%s)", humanLabel, interval.StartString(), q.CollectionName))
}

// MaxAllCPU selects the MAX of all metrics under 'cpu' per hour for nhosts hosts,
// e.g. in psuedo-SQL:
//
// SELECT MAX(metric1), ..., MAX(metricN)
// FROM cpu WHERE (hostname = '$HOSTNAME_1' OR ... OR hostname = '$HOSTNAME_N')
// AND time >= '$HOUR_START' AND time < '$HOUR_END'
// GROUP BY hour ORDER BY hour
func (d *MongoDevops) MaxAllCPU(qi query.Query, nHosts int) {
	interval := d.interval.RandWindow(8 * time.Hour)
	hostnames := d.getRandomHosts(nHosts)
	docs := getTimeFilterDocs(interval)
	bucketNano := time.Hour.Nanoseconds()
	metrics := getCPUMetricsSlice(len(cpuMetrics))

	pipelineQuery := []bson.M{
		{
			"$match": bson.M{
				"measurement": "cpu",
				"tags.hostname": bson.M{
					"$in": hostnames,
				},
				"key_id": bson.M{
					"$in": docs,
				},
			},
		},
		{
			"$project": bson.M{
				"_id":    0,
				"events": 1,
				"key_id": 1,
				"tags":   "$tags.hostname",
			},
		},
	}
	pipelineQuery = append(pipelineQuery, getTimeFilterPipeline(interval)...)
	pipelineQuery = append(pipelineQuery, bson.M{
		"$project": bson.M{
			"time_bucket": bson.M{
				"$subtract": []interface{}{
					"$events.timestamp_ns",
					bson.M{"$mod": []interface{}{"$events.timestamp_ns", bucketNano}},
				},
			},
			"events": 1,
		},
	})

	group := bson.M{
		"$group": bson.M{
			"_id": "$time_bucket",
		},
	}
	resultMap := group["$group"].(bson.M)
	for _, metric := range metrics {
		resultMap["max_"+metric] = bson.M{"$max": "$events." + metric}
	}
	pipelineQuery = append(pipelineQuery, group)
	pipelineQuery = append(pipelineQuery, bson.M{"$sort": bson.M{"_id": 1}})

	humanLabel := fmt.Sprintf("Mongo max cpu all fields, rand %4d hosts, rand 8hr by 1h", nHosts)
	q := qi.(*query.Mongo)
	q.HumanLabel = []byte(humanLabel)
	q.BsonDoc = pipelineQuery
	q.CollectionName = []byte("point_data")
	q.HumanDescription = []byte(fmt.Sprintf("%s: %s", humanLabel, interval.StartString()))
}

// GroupByTimeAndPrimaryTag selects the AVG of numMetrics metrics under 'cpu' per device per hour for a day,
// e.g. in psuedo-SQL:
//
// SELECT AVG(metric1), ..., AVG(metricN)
// FROM cpu
// WHERE time >= '$HOUR_START' AND time < '$HOUR_END'
// GROUP BY hour, hostname ORDER BY hour, hostname
func (d *MongoDevops) GroupByTimeAndPrimaryTag(qi query.Query, numMetrics int) {
	interval := d.interval.RandWindow(doubleGroupByDuration)
	metrics := getCPUMetricsSlice(numMetrics)
	docs := getTimeFilterDocs(interval)
	bucketNano := time.Hour.Nanoseconds()

	pipelineQuery := []bson.M{
		{
			"$match": bson.M{
				"measurement": "cpu",
				"key_id": bson.M{
					"$in": docs,
				},
			},
		},
		{
			"$project": bson.M{
				"_id":         0,
				"events":      1,
				"key_id":      1,
				"measurement": 1,
				"tags":        "$tags.hostname",
			},
		},
	}

	pipelineQuery = append(pipelineQuery, getTimeFilterPipeline(interval)...)
	pipelineQuery = append(pipelineQuery, []bson.M{
		{
			"$project": bson.M{
				"time_bucket": bson.M{
					"$subtract": []interface{}{
						"$events.timestamp_ns",
						bson.M{"$mod": []interface{}{"$events.timestamp_ns", bucketNano}},
					},
				},
				"measurement": 1,
				"tags":        1,
				"events":      1,
			},
		},
	}...)

	// Add groupby operator
	group := bson.M{
		"$group": bson.M{
			"_id": bson.M{
				"time":     "$time_bucket",
				"hostname": "$tags",
			},
		},
	}
	resultMap := group["$group"].(bson.M)
	for _, metric := range metrics {
		resultMap["avg_"+metric] = bson.M{"$avg": "$events." + metric}
	}
	pipelineQuery = append(pipelineQuery, group)

	// Add sort operators
	pipelineQuery = append(pipelineQuery, []bson.M{
		{"$sort": bson.M{"_id.hostname": 1}},
		{"$sort": bson.M{"_id.time": 1}},
	}...)

	humanLabel := fmt.Sprintf("Mongo mean of %d metrics, all hosts, rand 1day by 1hr", numMetrics)
	q := qi.(*query.Mongo)
	q.HumanLabel = []byte(humanLabel)
	q.BsonDoc = pipelineQuery
	q.CollectionName = []byte("point_data")
	q.HumanDescription = []byte(fmt.Sprintf("%s: %s (%s)", humanLabel, interval.StartString(), q.CollectionName))
}

// HighCPUForHosts populates a query that gets CPU metrics when the CPU has high
// usage between a time period for a number of hosts (if 0, it will search all hosts),
// e.g. in psuedo-SQL:
//
// SELECT * FROM cpu
// WHERE usage_user > 90.0
// AND time >= '$TIME_START' AND time < '$TIME_END'
// AND (hostname = '$HOST' OR hostname = '$HOST2'...)
func (d *MongoDevops) HighCPUForHosts(qi query.Query, nHosts int) {
	interval := d.interval.RandWindow(highCPUDuration)
	hostnames := d.getRandomHosts(nHosts)
	docs := getTimeFilterDocs(interval)

	pipelineQuery := []bson.M{}

	// Must match in the documents that correspond to time, as well as optionally
	// filter on those with the correct host if nHosts > 0
	match := bson.M{
		"$match": bson.M{
			"measurement": "cpu",
			"key_id": bson.M{
				"$in": docs,
			},
		},
	}
	if nHosts > 0 {
		matchMap := match["$match"].(bson.M)
		matchMap["tags.hostname"] = bson.M{"$in": hostnames}
	}

	pipelineQuery = append(pipelineQuery, []bson.M{
		match,
		bson.M{
			"$project": bson.M{
				"_id":    0,
				"events": 1,
				"key_id": 1,
				"tags":   "$tags.hostname",
			},
		},
	}...)

	pipelineQuery = append(pipelineQuery, getTimeFilterPipeline(interval)...)
	pipelineQuery = append(pipelineQuery, bson.M{
		"$match": bson.M{
			"events.usage_user": bson.M{"$gt": 90.0},
		},
	})

	humanLabel := getHighCPULabel("Mongo", nHosts)
	q := qi.(*query.Mongo)
	q.HumanLabel = []byte(humanLabel)
	q.BsonDoc = pipelineQuery
	q.CollectionName = []byte("point_data")
	q.HumanDescription = []byte(fmt.Sprintf("%s: %s (%s)", humanLabel, interval.StartString(), q.CollectionName))
}

// LastPointPerHost finds the last row for every host in the dataset
func (d *MongoDevops) LastPointPerHost(qi query.Query) {
	pipelineQuery := []bson.M{
		{"$match": bson.M{"measurement": "cpu"}},
		{
			"$group": bson.M{
				"_id":      bson.M{"hostname": "$tags.hostname"},
				"last_doc": bson.M{"$max": "$key_id"},
			},
		},
		{
			"$group": bson.M{
				"_id":   bson.M{"doc_key": "$last_doc"},
				"hosts": bson.M{"$addToSet": "$_id.hostname"},
			},
		},
		{
			"$lookup": bson.M{
				"from": "point_data",
				"let":  bson.M{"key_id": "$_id.doc_key", "hostnames": "$hosts", "measurement": "$measurement"},
				"pipeline": []bson.M{
					{
						"$match": bson.M{
							"$expr": bson.M{
								"$and": []bson.M{
									{"$in": []interface{}{"$tags.hostname", "$$hostnames"}},
									{"$eq": []interface{}{"$key_id", "$$key_id"}},
									{"$eq": []interface{}{"$measurement", "$$measurement"}},
								},
							},
						},
					},
				},
				"as": "allDocs",
			},
		},
		{"$unwind": "$allDocs"},
		{"$unwind": "$allDocs.events"},
		{
			"$project": bson.M{
				"key_id": "$allDocs.key_id",
				"tags":   "$allDocs.tags",
				"events": bson.M{
					"$filter": bson.M{
						"input": "$allDocs.events",
						"as":    "event",
						"cond": bson.M{
							"$and": []interface{}{
								bson.M{
									"$gte": []interface{}{
										"$$event.timestamp_ns",
										0,
									},
								},
							},
						},
					},
				},
			},
		},
		{"$unwind": "$events"},
		{
			"$group": bson.M{
				"_id":    bson.M{"hostname": "$tags.hostname"},
				"result": bson.M{"$last": "$events"},
			},
		},
	}

	humanLabel := "Mongo last row per host"
	q := qi.(*query.Mongo)
	q.HumanLabel = []byte(humanLabel)
	q.BsonDoc = pipelineQuery
	q.CollectionName = []byte("point_data")
	q.HumanDescription = []byte(fmt.Sprintf("%s", humanLabel))
}

// GroupByOrderByLimit populates a query.Query that has a time WHERE clause, that groups by a truncated date, orders by that date, and takes a limit:
// SELECT date_trunc('minute', time) AS t, MAX(cpu) FROM cpu
// WHERE time < '$TIME'
// GROUP BY t ORDER BY t DESC
// LIMIT $LIMIT
func (d *MongoDevops) GroupByOrderByLimit(qi query.Query) {
	interval := d.interval.RandWindow(time.Hour)
	interval = NewTimeInterval(d.interval.Start, interval.End)
	docs := getTimeFilterDocs(interval)
	bucketNano := time.Minute.Nanoseconds()

	pipelineQuery := []bson.M{
		{
			"$match": bson.M{
				"measurement": "cpu",
				"key_id": bson.M{
					"$in": docs,
				},
			},
		},
		{
			"$project": bson.M{
				"_id":    0,
				"events": 1,
				"key_id": 1,
				"tags":   "$tags.hostname",
			},
		},
	}
	pipelineQuery = append(pipelineQuery, getTimeFilterPipeline(interval)...)
	pipelineQuery = append(pipelineQuery, []bson.M{
		{
			"$project": bson.M{
				"time_bucket": bson.M{
					"$subtract": []interface{}{
						"$events.timestamp_ns",
						bson.M{"$mod": []interface{}{"$events.timestamp_ns", bucketNano}},
					},
				},
				"field": "$events.usage_user",
			},
		},
		{
			"$group": bson.M{
				"_id":       "$time_bucket",
				"max_value": bson.M{"$max": "$field"},
			},
		},
		{"$sort": bson.M{"_id": -1}},
		{"$limit": 5},
	}...)

	humanLabel := "Mongo max cpu over last 5 min-intervals (rand end)"
	q := qi.(*query.Mongo)
	q.HumanLabel = []byte(humanLabel)
	q.BsonDoc = pipelineQuery
	q.CollectionName = []byte("point_data")
	q.HumanDescription = []byte(fmt.Sprintf("%s: %s", humanLabel, interval.EndString()))
}
