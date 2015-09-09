package main

import (
	"fmt"
	"math"
	"math/rand"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	cparse "github.com/SoftwareDefinedBuildings/sync2_quasar/configparser"
	cpint "github.com/SoftwareDefinedBuildings/btrdb/cpinterface"
	capnp "github.com/glycerine/go-capnproto"
	uuid "code.google.com/p/go-uuid/uuid"
)

var (
	TOTAL_RECORDS int64
	TCP_CONNECTIONS int
	POINTS_PER_MESSAGE uint32
	NANOS_BETWEEN_POINTS int64
	NUM_SERVERS int
	NUM_STREAMS int
	FIRST_TIME int64
	RAND_SEED int64
	PERM_SEED int64
	MAX_TIME_RANDOM_OFFSET float64
	DETERMINISTIC_KV bool
)

var (
	VERIFY_RESPONSES = false
)

var points_sent uint32 = 0

var points_received uint32 = 0

var points_verified uint32 = 0

type ConnectionID struct {
    serverIndex int
    connectionIndex int
}

type InsertMessagePart struct {
	segment *capnp.Segment
	request *cpint.Request
	insert *cpint.CmdInsertValues
	recordList *cpint.Record_List
	pointerList *capnp.PointerList
	record *cpint.Record
}

var insertPool sync.Pool = sync.Pool{
	New: func () interface{} {
		var seg *capnp.Segment = capnp.NewBuffer(nil)
		var req cpint.Request = cpint.NewRootRequest(seg)
		var insert cpint.CmdInsertValues = cpint.NewCmdInsertValues(seg)
		insert.SetSync(false)
		var recList cpint.Record_List = cpint.NewRecordList(seg, int(POINTS_PER_MESSAGE))
		var pointList capnp.PointerList = capnp.PointerList(recList)
		var record cpint.Record = cpint.NewRecord(seg)
		return InsertMessagePart{
			segment: seg,
			request: &req,
			insert: &insert,
			recordList: &recList,
			pointerList: &pointList,
			record: &record,
		}
	},
}

var get_time_value func (int64, *rand.Rand) float64

func getRandValue (time int64, randGen *rand.Rand) float64 {
	// We technically don't need time anymore, but if we switch back to a sine wave later it's useful to keep it around as a parameter
	return randGen.NormFloat64()
}

var sines [100]float64

var sinesIndex = 100
func getSinusoidValue (time int64, randGen *rand.Rand) float64 {
    sinesIndex = (sinesIndex + 1) % 100;
    return sines[sinesIndex];
}

func min64 (x1 int64, x2 int64) int64 {
	if x1 < x2 {
		return x1
	} else {
		return x2
	}
}

func insert_data(uuid []byte, start *int64, connection net.Conn, sendLock *sync.Mutex, connID ConnectionID, response chan ConnectionID, streamID int, cont chan uint32, randGen *rand.Rand, permutation []int64, numMessages int64) {
	var time int64 = *start
	var endTime int64
	var numPoints uint32
	if TOTAL_RECORDS < 0 {
		endTime = 0x7FFFFFFFFFFFFFFF
	} else {
		endTime = min64(time + TOTAL_RECORDS * NANOS_BETWEEN_POINTS, 0x7FFFFFFFFFFFFFFF)
	}
	var j int64
	for j = 0; j < numMessages; j++ {
		time = permutation[j]
		
		numPoints = POINTS_PER_MESSAGE
			
		var mp InsertMessagePart = insertPool.Get().(InsertMessagePart)
		
		segment := mp.segment
		request := mp.request
		insert := mp.insert
		recordList := mp.recordList;
		pointerList := mp.pointerList
		record := *mp.record
		
		request.SetEchoTag(uint64(streamID))
		insert.SetUuid(uuid)

		if endTime - time < int64(POINTS_PER_MESSAGE) * NANOS_BETWEEN_POINTS {
			numPoints = uint32((endTime - time) / NANOS_BETWEEN_POINTS)
			rlst := cpint.NewRecordList(segment, int(numPoints))
			recordList = &rlst;
			plst := capnp.PointerList(rlst)
			pointerList = &plst;
		}
		
		cont <- numPoints // Blocks if we haven't received enough responses

		var i int
		for i = 0; uint32(i) < numPoints; i++ {
		    if DETERMINISTIC_KV {
		    	record.SetTime(time)
		    } else {
				record.SetTime(time + int64(randGen.Float64() * MAX_TIME_RANDOM_OFFSET))
			}
			record.SetValue(get_time_value(time, randGen))
			pointerList.Set(i, capnp.Object(record))
			time += NANOS_BETWEEN_POINTS
		}
		insert.SetValues(*recordList)
		request.SetInsertValues(*insert)
		
		var sendErr error
		
		sendLock.Lock()
		_, sendErr = segment.WriteTo(connection)
		sendLock.Unlock()

		insertPool.Put(mp)
		
		if sendErr != nil {
			fmt.Printf("Error in sending request: %v\n", sendErr)
			return
		}
		atomic.AddUint32(&points_sent, uint32(numPoints))
	}
	response <- connID
}

type QueryMessagePart struct {
	segment *capnp.Segment
	request *cpint.Request
	query *cpint.CmdQueryStandardValues
}

var queryPool sync.Pool = sync.Pool{
	New: func () interface{} {
		var seg *capnp.Segment = capnp.NewBuffer(nil)
		var req cpint.Request = cpint.NewRootRequest(seg)
		var query cpint.CmdQueryStandardValues = cpint.NewCmdQueryStandardValues(seg)
		query.SetVersion(0)
		req.SetQueryStandardValues(query)
		return QueryMessagePart{
			segment: seg,
			request: &req,
			query: &query,
		}
	},
}

func query_data(uuid []byte, start *int64, connection net.Conn, sendLock *sync.Mutex, connID ConnectionID, response chan ConnectionID, streamID int, cont chan uint32, randGen *rand.Rand, permutation []int64, numMessages int64) {
	var time int64 = *start
	var endTime int64
	var numPoints uint32
	if TOTAL_RECORDS < 0 {
		endTime = 0x7FFFFFFFFFFFFFFF
	} else {
		endTime = min64(time + TOTAL_RECORDS * NANOS_BETWEEN_POINTS, 0x7FFFFFFFFFFFFFFF)
	}
	var j int64
	for j = 0; j < numMessages; j++ {
	    time = permutation[j]
	    
	    numPoints = POINTS_PER_MESSAGE
	    
		var mp QueryMessagePart = queryPool.Get().(QueryMessagePart)
		
		segment := mp.segment
		request := mp.request
		query := mp.query
		
		request.SetEchoTag(uint64(streamID))
		query.SetUuid(uuid)
		query.SetStartTime(time)
		if endTime - time < int64(POINTS_PER_MESSAGE) * NANOS_BETWEEN_POINTS {
			numPoints = uint32((endTime - time) / NANOS_BETWEEN_POINTS)
		}
		
		cont <- numPoints // Blocks if we haven't received enough responses
		
		query.SetEndTime(time + NANOS_BETWEEN_POINTS * int64(numPoints))

		var sendErr error
		
		sendLock.Lock()
		_, sendErr = segment.WriteTo(connection)
		sendLock.Unlock()

		queryPool.Put(mp)
		
		if sendErr != nil {
			fmt.Printf("Error in sending request: %v\n", sendErr)
			os.Exit(1)
		}
		atomic.AddUint32(&points_sent, uint32(numPoints))
	}
	response <- connID
}

func validateResponses(connection net.Conn, connLock *sync.Mutex, idToChannel []chan uint32, randGens []*rand.Rand, times []int64, receivedCounts []uint32, pass *bool, numUsing *int) {
	for true {
		/* I've restructured the code so that this is the only goroutine that receives from the connection.
		   So, the locks aren't necessary anymore. But, I've kept the lock around in case we switch to a different
		   design later on. */
		//connLock.Lock()
		responseSegment, respErr := capnp.ReadFromStream(connection, nil)
		//connLock.Unlock()
		
		if *numUsing == 0 {
			return
		}
	
		if respErr != nil {
			fmt.Printf("Error in receiving response: %v\n", respErr)
			os.Exit(1)
		}
	
		responseSeg := cpint.ReadRootResponse(responseSegment)
		id := responseSeg.EchoTag()
		status := responseSeg.StatusCode()
		
		var channel chan uint32 = idToChannel[id]
		
		var numPoints uint32
		
		if (responseSeg.Final()) {
		    numPoints = <-channel
			atomic.AddUint32(&points_received, numPoints)
		}
		
		if status != cpint.STATUSCODE_OK {
			fmt.Printf("Quasar returns status code %s!\n", status)
			os.Exit(1)
		}

		if VERIFY_RESPONSES {
   			var randGen *rand.Rand = randGens[id]
			var currTime int64 = times[id]
			var expTime int64
			records := responseSeg.Records().Values()
			var num_records uint32 = uint32(records.Len())
			var expected float64 = 0
			var received float64 = 0
			var recTime int64 = 0
			if responseSeg.Final() {
			    if num_records + receivedCounts[id] != numPoints {
					fmt.Printf("Expected %v points in query response, but got %v points instead.\n", numPoints, num_records)
					*pass = false
				}
				receivedCounts[id] = 0
			} else {
				receivedCounts[id] += num_records
			}
			for m := 0; uint32(m) < num_records; m++ {
				received = records.At(m).Value()
				recTime = records.At(m).Time()
				if DETERMINISTIC_KV {
					expTime = currTime
				} else {
					expTime = currTime + int64(randGen.Float64() * MAX_TIME_RANDOM_OFFSET)
				}
				expected = get_time_value(recTime, randGens[id])
				if expTime == recTime && received == expected {
					atomic.AddUint32(&points_verified, uint32(1))
				} else {
					fmt.Printf("Expected (%v, %v), got (%v, %v)\n", expTime, expected, recTime, received)
					*pass = false
				}
				currTime = currTime + NANOS_BETWEEN_POINTS
			}
			times[id] = currTime;
		}
	}
}

type DeleteMessagePart struct {
	segment *capnp.Segment
	request *cpint.Request
	query *cpint.CmdDeleteValues
}

var deletePool sync.Pool = sync.Pool{
	New: func () interface{} {
		var seg *capnp.Segment = capnp.NewBuffer(nil)
		var req cpint.Request = cpint.NewRootRequest(seg)
		var query cpint.CmdDeleteValues = cpint.NewCmdDeleteValues(seg)
		req.SetEchoTag(0)
		return DeleteMessagePart{
			segment: seg,
			request: &req,
			query: &query,
		}
	},
}

func delete_data(uuid []byte, connection net.Conn, sendLock *sync.Mutex, recvLock *sync.Mutex, startTime int64, endTime int64, connID ConnectionID, response chan ConnectionID) {
	var mp DeleteMessagePart = deletePool.Get().(DeleteMessagePart)
	segment := *mp.segment
	request := *mp.request
	query := *mp.query
	
	query.SetUuid(uuid)
	query.SetStartTime(startTime)
	query.SetEndTime(endTime)
	request.SetDeleteValues(query)
	
	(*sendLock).Lock()
	_, sendErr := segment.WriteTo(connection)
	(*sendLock).Unlock()
	
	deletePool.Put(mp)
	
	if sendErr != nil {
		fmt.Printf("Error in sending request: %v\n", sendErr)
		os.Exit(1)
	}
	
	(*recvLock).Lock()
	responseSegment, respErr := capnp.ReadFromStream(connection, nil)
	(*recvLock).Unlock()
	
	if respErr != nil {
		fmt.Printf("Error in receiving response: %v\n", respErr)
		os.Exit(1)
	}

	responseSeg := cpint.ReadRootResponse(responseSegment)
	status := responseSeg.StatusCode()
	
	if status != cpint.STATUSCODE_OK {
		fmt.Printf("Quasar returns status code %s!\n", status)
	}
	
	response <- connID
}

func getIntFromConfig(key string, config map[string]interface{}) int64 {
	elem, ok := config[key]
	if !ok {
		fmt.Printf("Could not read %v from config file\n", key)
		os.Exit(1)
	}
	intval, err := strconv.ParseInt(elem.(string), 0, 64)
	if err != nil {
		fmt.Printf("Could not parse %v to an int64: %v\n", elem, err)
		os.Exit(1)
	}
	return intval
}

func getServer(uuid []byte) int {
	return int(uint(uuid[0]) % uint(NUM_SERVERS))
}

func main() {
	args := os.Args[1:]
	var send_messages func([]byte, *int64, net.Conn, *sync.Mutex, ConnectionID, chan ConnectionID, int, chan uint32, *rand.Rand, []int64, int64)
	var DELETE_POINTS bool = false
	
	if len(args) > 0 && args[0] == "-i" {
		fmt.Println("Insert mode");
		send_messages = insert_data
	} else if len(args) > 0 && args[0] == "-q" {
		fmt.Println("Query mode");
		send_messages = query_data
	} else if len(args) > 0 && args[0] == "-v" {
		fmt.Println("Query mode with verification");
		send_messages = query_data
		VERIFY_RESPONSES = true
	} else if len(args) > 0 && args[0] == "-d" {
		fmt.Println("Delete mode")
		DELETE_POINTS = true
	} else {
		fmt.Println("Usage: use -i to insert data and -q to query data. To query data and verify the response, use the -v flag instead of the -q flag. Use the -d flag to delete data. To get a CPU profile, add a file name after -i, -v, or -q.");
		return
	}
	
	/* Check if the user has requested a CPU Profile. */
	if len(args) > 1 {
		f, err := os.Create(args[1])
		if err != nil {
			fmt.Println(err)
			return;
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	
	/* Read the configuration file. */
	
	configfile, err := ioutil.ReadFile("loadConfig.ini")
	if err != nil {
		fmt.Printf("Could not read loadConfig.ini: %v\n", err)
		return
	}
	
	config, isErr := cparse.ParseConfig(string(configfile))
	if isErr {
		fmt.Println("There were errors while parsing loadConfig.ini. See above.")
		return
	}
	
	TOTAL_RECORDS = getIntFromConfig("TOTAL_RECORDS", config)
	TCP_CONNECTIONS = int(getIntFromConfig("TCP_CONNECTIONS", config))
	POINTS_PER_MESSAGE = uint32(getIntFromConfig("POINTS_PER_MESSAGE", config))
	NANOS_BETWEEN_POINTS = getIntFromConfig("NANOS_BETWEEN_POINTS", config)
	NUM_SERVERS = int(getIntFromConfig("NUM_SERVERS", config))
	NUM_STREAMS = int(getIntFromConfig("NUM_STREAMS", config))
	FIRST_TIME = getIntFromConfig("FIRST_TIME", config)
	RAND_SEED = getIntFromConfig("RAND_SEED", config)
	PERM_SEED = getIntFromConfig("PERM_SEED", config)
	var maxConcurrentMessages int64 = getIntFromConfig("MAX_CONCURRENT_MESSAGES", config);
	var timeRandOffset int64 = getIntFromConfig("MAX_TIME_RANDOM_OFFSET", config)
	if TOTAL_RECORDS <= 0 || TCP_CONNECTIONS <= 0 || POINTS_PER_MESSAGE <= 0 || NANOS_BETWEEN_POINTS <= 0 || NUM_STREAMS <= 0 || maxConcurrentMessages <= 0 {
		fmt.Println("TOTAL_RECORDS, TCP_CONNECTIONS, POINTS_PER_MESSAGE, NANOS_BETWEEN_POINTS, NUM_STREAMS, and MAX_CONCURRENT_MESSAGES must be positive.")
		os.Exit(1)
	}
	if timeRandOffset >= NANOS_BETWEEN_POINTS {
		fmt.Println("MAX_TIME_RANDOM_OFFSET must be less than NANOS_BETWEEN_POINTS.")
		os.Exit(1)
	}
	if timeRandOffset > (1 << 53) { // must be exactly representable as a float64
		fmt.Println("MAX_TIME_RANDOM_OFFSET is too large: the maximum value is 2 ^ 53.")
		os.Exit(1)
	}
	if timeRandOffset < 0 {
		fmt.Println("MAX_TIME_RANDOM_OFFSET must be nonnegative.")
		os.Exit(1)
	}
	if VERIFY_RESPONSES && maxConcurrentMessages > 1 {
		fmt.Println("WARNING: MAX_CONCURRENT_MESSAGES is always 1 when verifying responses.");
		maxConcurrentMessages = 1;
	}
	if VERIFY_RESPONSES && PERM_SEED != 0 && !DETERMINISTIC_KV {
		fmt.Println("ERROR: PERM_SEED must be set to 0 when verifying nondeterministic responses.");
		return;
	}
	MAX_TIME_RANDOM_OFFSET = float64(timeRandOffset)
	DETERMINISTIC_KV = (config["DETERMINISTIC_KV"].(string) == "true")
	
	if DETERMINISTIC_KV {
		get_time_value = getSinusoidValue;
		for r := 0; r < 100; r++ {
			sines[r] = math.Sin(2 * math.Pi * float64(r) / 100)
		}
	} else {
		get_time_value = getRandValue;
	}
	
	var seedGen *rand.Rand = rand.New(rand.NewSource(RAND_SEED))
	var permGen *rand.Rand = rand.New(rand.NewSource(PERM_SEED));
	var randGens []*rand.Rand = make([]*rand.Rand, NUM_STREAMS)
	
	var j int
	var ok bool
	var dbAddrStr interface{}
	var dbAddrs []string = make([]string, NUM_SERVERS)
	for j = 0; j < NUM_SERVERS; j++ {
		dbAddrStr, ok = config[fmt.Sprintf("DB_ADDR%v", j + 1)]
		if !ok {
			break
		}
		dbAddrs[j] = dbAddrStr.(string)
	}
	_, ok = config[fmt.Sprintf("DB_ADDR%v", j + 1)]
	if j != NUM_SERVERS || ok {
		fmt.Println("The number of specified DB_ADDRs must equal NUM_SERVERS.")
		os.Exit(1)
	}
	
	var uuids [][]byte = make([][]byte, NUM_STREAMS)
	
	var uuidStr interface{}
	var uuidParsed uuid.UUID
	for j = 0; j < NUM_STREAMS; j++ {
		uuidStr, ok = config[fmt.Sprintf("UUID%v", j + 1)]
		if !ok {
			break
		}
		uuidParsed = uuid.Parse(uuidStr.(string))
		if uuidParsed == nil {
			fmt.Printf("Invalid UUID %v\n", uuidStr)
			os.Exit(1)
		}
		uuids[j] = []byte(uuidParsed)
	}
	_, ok = config[fmt.Sprintf("UUID%v", j + 1)]
	if j != NUM_STREAMS || ok {
		fmt.Println("The number of specified UUIDs must equal NUM_STREAMS.")
		os.Exit(1)
	}
	fmt.Printf("Using UUIDs ")
	for j = 0; j < NUM_STREAMS; j++ {
		fmt.Printf("%s ", uuid.UUID(uuids[j]).String())
	}
	fmt.Printf("\n")
	
	runtime.GOMAXPROCS(runtime.NumCPU())
	var connections [][]net.Conn = make([][]net.Conn, NUM_SERVERS)
	var sendLocks [][]*sync.Mutex = make([][]*sync.Mutex, NUM_SERVERS)
	var recvLocks [][]*sync.Mutex = make([][]*sync.Mutex, NUM_SERVERS)
	
	for s := range dbAddrs {
		fmt.Printf("Creating connections to %v...\n", dbAddrs[s])
	    connections[s] = make([]net.Conn, TCP_CONNECTIONS)
	    sendLocks[s] = make([]*sync.Mutex, TCP_CONNECTIONS)
	    recvLocks[s] = make([]*sync.Mutex, TCP_CONNECTIONS)
		for i := range connections[s] {
			connections[s][i], err = net.Dial("tcp", dbAddrs[s])
			if err == nil {
				fmt.Printf("Created connection %v to %v\n", i, dbAddrs[s])
				sendLocks[s][i] = &sync.Mutex{}
				recvLocks[s][i] = &sync.Mutex{}
			} else {
				fmt.Printf("Could not connect to database: %s\n", err)
				os.Exit(1);
			}
		}
	}
	fmt.Println("Finished creating connections")

	var serverIndex int = 0
	var streamCounts []int = make([]int, NUM_SERVERS)
	var connIndex int

	var sig chan ConnectionID = make(chan ConnectionID)
	var usingConn [][]int = make([][]int, NUM_SERVERS)
	for y := 0; y < NUM_SERVERS; y++ {
		usingConn[y] = make([]int, TCP_CONNECTIONS)
	}
	var idToChannel []chan uint32 = make([]chan uint32, NUM_STREAMS)
	var cont chan uint32
	var randGen *rand.Rand
	var startTimes []int64 = make([]int64, NUM_STREAMS)
	var verification_test_pass bool = true
	var perm [][]int64 = make([][]int64, NUM_STREAMS)
	var pointsReceived []uint32
	if VERIFY_RESPONSES {
		pointsReceived = make([]uint32, NUM_STREAMS)
	} else {
		pointsReceived = nil
	}
	
	var perm_size = int64(math.Ceil(float64(TOTAL_RECORDS) / float64(POINTS_PER_MESSAGE)))
	var f int64
	for e := 0; e < NUM_STREAMS; e++ {
		perm[e] = make([]int64, perm_size)
	    if PERM_SEED == 0 {
			for f = 0; f < perm_size; f++ {
				perm[e][f] = FIRST_TIME + NANOS_BETWEEN_POINTS * int64(POINTS_PER_MESSAGE) * f
			}
		} else {
			x := permGen.Perm(int(perm_size))
			for f = 0; f < perm_size; f++ {
				perm[e][f] = FIRST_TIME + NANOS_BETWEEN_POINTS * int64(POINTS_PER_MESSAGE) * int64(x[f])
			}
		}
	}
	fmt.Println("Finished generating insert/query order");
	
	var startTime int64 = time.Now().UnixNano()
	if DELETE_POINTS {
		for g := 0; g < NUM_STREAMS; g++ {
			serverIndex = getServer(uuids[g])
			connIndex = streamCounts[serverIndex] % TCP_CONNECTIONS
			go delete_data(uuids[g], connections[serverIndex][connIndex], sendLocks[serverIndex][connIndex], recvLocks[serverIndex][connIndex], FIRST_TIME, FIRST_TIME + NANOS_BETWEEN_POINTS * TOTAL_RECORDS, ConnectionID{serverIndex, connIndex}, sig)
			streamCounts[serverIndex]++
		}
	} else {
		for z := 0; z < NUM_STREAMS; z++ {
			cont = make(chan uint32, maxConcurrentMessages)
			idToChannel[z] = cont
			randGen = rand.New(rand.NewSource(seedGen.Int63()))
			randGens[z] = randGen
			startTimes[z] = FIRST_TIME
			serverIndex = getServer(uuids[z])
			connIndex = streamCounts[serverIndex] % TCP_CONNECTIONS
			go send_messages(uuids[z], &startTimes[z], connections[serverIndex][connIndex], sendLocks[serverIndex][connIndex], ConnectionID{serverIndex, connIndex}, sig, z, cont, randGen, perm[z], perm_size)
			usingConn[serverIndex][connIndex]++
			streamCounts[serverIndex]++
		}
	
		for serverIndex = 0; serverIndex < NUM_SERVERS; serverIndex++ {
			for connIndex = 0; connIndex < TCP_CONNECTIONS; connIndex++ {
				go validateResponses(connections[serverIndex][connIndex], recvLocks[serverIndex][connIndex], idToChannel, randGens, startTimes, pointsReceived, &verification_test_pass, &usingConn[serverIndex][connIndex])
			}
		}
		
		/* Handle ^C */
		interrupt := make(chan os.Signal)
		signal.Notify(interrupt, os.Interrupt)
		go func() {
			<-interrupt // block until an interrupt happens
			fmt.Println("\nDetected ^C. Abruptly ending program...")
			fmt.Println("The following are the start times of the messages that are currently being inserted/queried:")
			for i := 0; i < NUM_STREAMS; i++ {
				fmt.Printf("%v: %v\n", uuid.UUID(uuids[i]).String(), startTimes[i])
			}
			os.Exit(0)
		}()

		go func () {
			for {
				time.Sleep(time.Second)
				fmt.Printf("Sent %v, ", points_sent)
				atomic.StoreUint32(&points_sent, 0)
				fmt.Printf("Received %v\n", points_received)
				atomic.StoreUint32(&points_received, 0)
				points_received = 0
			}
		}()
	}

	var response ConnectionID
	for k := 0; k < NUM_STREAMS; k++ {
		response = <-sig
	    serverIndex = response.serverIndex
	    connIndex = response.connectionIndex
		usingConn[serverIndex][connIndex]--
		if usingConn[serverIndex][connIndex] == 0 {
			connections[serverIndex][connIndex].Close()
			fmt.Printf("Closed connection %v to server %v\n", connIndex, dbAddrs[serverIndex])
		}
	}
	
	var deltaT int64 = time.Now().UnixNano() - startTime
	
	// Close unused connections
	/*
	for k := NUM_STREAMS; k < TCP_CONNECTIONS; k++ {
		connections[k].Close()
		fmt.Printf("Closed connection %v to server\n", k)
	}
	*/
    
	if !DELETE_POINTS {
		fmt.Printf("Sent %v, Received %v\n", points_sent, points_received)
	}
	if (VERIFY_RESPONSES) {
		fmt.Printf("%v points are verified to be correct\n", points_verified);
		if verification_test_pass {
			fmt.Println("All points were verified to be correct. Test PASSes.")
		} else {
			fmt.Println("Some points were found to be incorrect. Test FAILs.")
			os.Exit(1) // terminate with a non-zero exit code
		}
	} else {
		fmt.Println("Finished")
	}
	fmt.Printf("Total time: %d nanoseconds for %d points\n", deltaT, TOTAL_RECORDS * int64(NUM_STREAMS))
	fmt.Printf("Average: %d nanoseconds per point (floored to integer value)\n", deltaT / (TOTAL_RECORDS * int64(NUM_STREAMS)))
	fmt.Println(deltaT)
}
