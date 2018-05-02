# V3IO-TSDB
iguazio API lib for time-series DB access and Prometheus TSDB storage driver. 

> Note: This project is still under development and requiers the latest 1.7 release of iguazio DB (with Blob functions).

## Overview
iguazio provides a real-time flexible document database engine which accelerates popular BigData and open-source 
frameworks such as Spark and Presto, as well as provides AWS compatible data APIs (DynamoDB, Kinesis, S3). 

iguazio's DB engine runs at the speed of in-memory databases, but uses lower cost and higher density (NVMe) Flash. It has 
a unique low-level design with highly parallel processing and OS bypass which treats Flash as async memory pages. 

iguazio's DB low-level APIs (v3io) have rich API semantics and multiple indexing types which allow it to run multiple
workloads and processing engines on exactly the same data and consistently read/write the data in different tools.

This project uses v3io semantics (row & col layouts, arrays, random & sequential indexes, etc.) to provide an extremely
fast and scalable time-series database engine which can be accessed simultaneously by multiple engines and APIs, such as:
- Prometheus TimeSeries DB (for metrics scraping & queries)
- [nuclio](https://github.com/nuclio/nuclio) serverless functions (for real-time ingestion, stream processing or queries) 
- iguazio DynamoDB API (with extensions) 
- Apache Presto & Spark (future item, for SQL & AI)

[nuclio](https://github.com/nuclio/nuclio) supports HTTP and a large variety of streaming/triggering options (Kafka, Kinesis, Azure event-hub, RabbitMQ, NATS, iguazio streams, MQTT, Cron tasks) and provides automatic deployment and auto-scaling 
enabeling ingestion from a variety of sources at endless scalability. nuclio functions can be customized to pre-process 
incoming data e.g. examin metric data, alert, convert formarts, etc.  

<br>

![architecture](timeseries.png)
<br>

## Architecture
The solution stores the raw data in highly compressed column chunks (using Gorilla/XOR compression variation), with one 
chunk for every n hours (1hr default). Queries will only retrieve and decompress the specific columns based on the 
requested time range. 

Users can define pre-aggregates (count, avg, sum, min, max, stddev, stdvar) which use update expressions and store
data consistently in arrays per user defined intervals (RollupMin) and/or dimensions (labels). 

High-resolution queries will detect the pre-aggregates automatically and selectively access the array ranges 
(skip chunk retrieval, decompression and aggregation) which significantly accelerates searches and provides real-time 
response. An extension supports overlapping aggregates (retrieve last 1hr, 6h, 12hr, 24hr stats in a single request), 
which is currently not possible via the standard Prometheus TSDB API.  

The data can be partitioned to multiple tables (e.g. one per week) or use a cyclic table (goes back to the first chunk after
 reaching the end) and multiple tables are stored in a hierarchy under the specified path. 
 
Metric names and labels are stored in search optimized keys and string attributes. The iguazio DB engine can run a full 
dimension scan (searches) at the rate of millions of metrics per second, or use selective range based queries to access 
a specific metric family. 

v3io random access keys (Hash based) allow real-time sample data ingestion/retrieval and stream processing. 

To maintain high-performance over low-speed connections implement auto IO throttling. If the link is slow, multiple 
samples will be pushed in a single operation and users can configure the maximum allowed batch (trade efficiency with 
consistency). IO is done using multiple parallel connections/workers enabling maximum throughput regardless of the 
link latency.       

## How To Use  

The code is separated to a Prometheus complient adapter in [/promtsdb](promtsdb) and a more generic/advanced adapter in 
[/pkg/tsdb](pkg/tsdb), use the later for custom functions and code. see a full usage example in 
[v3iotsdb_test.go](/pkg/tsdb/v3iotsdb_test.go), both have similar semantics.

For Prometheus, use the fork located in `https://github.com/v3io/prometheus`, which already loads this
library, and place a `v3io.yaml` file with relevant configuration in the same folder as the Prometheus
executable (see details on configurations below).

A developer using this library should first create a `NewV3ioAdapter`. By using the adapter he can create an `Appender` for 
adding samples or a `Querier` for querying the database and retrieving a set of metrics or aggregates. See the following 
sections for details.

To use nuclio functions see the function example under [\nuclio](nuclio)

### Creating and Configuring a TSDB Adapter 

The first step is to create an adapter to the TSDB with relevant configuration. The `NewV3ioAdapter` function accepts 3
parameters: the configuration structure, v3io data container object and logger object. The last 2 are optional in case
you already have container and logger (when using nuclio data bindings).

Configuration is specified in a YAML or JSON format and can be read from a file using `config.LoadConfig(path string)` 
or loaded from a local buffer using `config.LoadFromData(data []byte)`. See details on the configuration
options in [config](config/config.go). A minimal configuration looks like this: 

```yaml
v3ioUrl: "v3io address:port"
container: "tsdb"
path: "metrics"
```

> If you plan on using pre-aggregation to speed aggregate queries, specify the `Rollups` (function list) and 
`RollupMin` (bucket time in minutes) parameters. The supported aggregation functions are: count, sum, avg, min, max, 
stddev, stdvar.

Example of creating an adpapter:

```go
	// create configuration object from file
	cfg, err := config.LoadConfig("v3io.yaml")
	if err != nil {
		panic(err)
	}

	// create and start a new TSDB adapter 
	adapter := tsdb.NewV3ioAdapter(cfg, nil, nil)
	err = adapter.Start()
	if err != nil {
		panic(err)
	}
```

### Creating and Using an Appender (ingest metrics)

The `Appender` interface is used to ingest metrics data and there are two functions for it: `Add` and `AddFast` which can come
after using Add (using the refID returned by Add) to reduce lookup/hash overhead.

Example:

```go
	// create an Appender interface 
	appender, err := adapter.Appender()
	if err != nil {
		panic(err)
	}

	// create metrics labels, `__name__` lable specify the metric name (e.g. cpu, temperature, ..), the other labels can be
	// used in searches (filtering or grouping) or aggregations  
	lset := utils.Labels{utils.Label{Name: "__name__", Value: "http_req"},
		utils.Label{Name: "method", Value: "post"}}

	// Add a sample with current time (in milisec) and the value of 7.9
	ref, err := appender.Add(lset, time.Now().Unix * 1000, 7.9)
	if err != nil {
		panic(err)
	}

	// Add a second sample using AddFast and the refID from Add
	err := appender.AddFast(nil, ref, time.Now().Unix * 1000 + 1000, 8.3)
	if err != nil {
		panic(err)
	}
```

### Creating and Using a Querier (read metrics and aggregates) 

The `Querier` interface is used to query the database and return one or more metrics. You first need to create a `Querier`
and specify the query window (min and max times) and then use `Select()` or `SelectOverlap()` commands which will 
return a list of series (as an iterator object).

Every returned series has two interfaces; `Labels()` which returns the series or aggregator labels and `Iterator()`
which returns an iterator over the series or aggregator values.

The `Select()` call accepts 3 parameters:
* functions (string) - optional, a comma separated list of aggregation functions e.g. `"count,sum,avg,stddev"`
* step (int64) - optional, the step interval used for the aggregation functions in milisec 
* filter (string) - V3iO GetItems filter string for selecting the desired metrics e.g. `__name__=='http_req'`

Using `functions` and `step` is optional. Use it only when you are interested in pre-aggregation and the step is >> than 
the sampling interval (and preferably equal or greater than the partition RollupMin interval). When using aggregates, it will
return one series per aggregate function and the `Aggregator` label will be added to that series with the function name.

In some cases we want to retrieve overlapping aggregates instead of fixed interval ones, e.g. stats for last 1hr, 6hr, 24hr, and
the `SelectOverlap()` call adds the `win` integer array ([]int) which allows specifying the requested windows. The windows are 
multiplied by the step value and start from the querier maxt value e.g. for 1hr, 6hr, and 24hr windows. Use `Step=3600 * 1000` 
(1hr), `win=[1,6,24]`, and `maxt` as the current time. The result set (series iterator) in this case will only contain 3 
elements sorted from the oldest to newest (24, 6, 1).

Creating a querier:

```go
	qry, err := adapter.Querier(nil, minTime, maxTime)
	if err != nil {
		panic(err)
	}
```

Simple select example (no aggregates):
```go
	set, err := qry.Select("", 0, "__name__=='http_req'")
```

Select using aggregates:

```go
	set, err := qry.Select("count,avg,sum,max", 1000*3600, "__name__=='http_req'")
```

Using SelectOverlap (overlapping windows): 

```go
	set, err := qry.SelectOverlap("count,avg,sum", 1000*3600, []int{24,6,1}, "__name__=='http_req'")
```

Once we obtain a set using one of the methods above we can iterate over the set and the individual series in the following way:

```go
	for set.Next() {
		if set.Err() != nil {
			panic(set.Err())
		}

		series := set.At()
		fmt.Println("\nLables:", series.Labels())
		iter := series.Iterator()
		for iter.Next() {
			if iter.Err() != nil {
				panic(iter.Err())
			}

			t, v := iter.At()
			fmt.Printf("t=%d,v=%.2f ", t, v)
		}
		fmt.Println()
	}
```
