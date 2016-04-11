package mongotape

import (
	"fmt"
	mgo "github.com/10gen/llmgo"
	"github.com/10gen/llmgo/bson"
	"github.com/10gen/mongotape/mongoproto"
	"os"
	"testing"
	"time"
)

const (
	nonAuthTestServerUrl = "mongodb://localhost:27035"
	authTestServerUrl    = "mongodb://authorizedUser:authorizedPwd@localhost:27035/admin"
	testDB               = "mongotape"
	testCollection       = "test"
	testCursorId         = int64(12345)
	testSpeed            = float64(100)
)

var testTime = time.Now()
var currentTestServerUrl string
var authTestServerMode bool

//recordedOpGenerator maintains a pair of connection stubs and channel to allow
//ops to be generated by the driver and passed to a channel
type recordedOpGenerator struct {
	session          *SessionStub
	serverConnection *ConnStub
	opChan           chan *RecordedOp
}

func TestMain(m *testing.M) {
	if os.Getenv("AUTH") == "1" {
		currentTestServerUrl = authTestServerUrl
		authTestServerMode = true
	} else {
		currentTestServerUrl = nonAuthTestServerUrl
		authTestServerMode = false
	}
	os.Exit(m.Run())

}

func newRecordedOpGenerator() *recordedOpGenerator {
	session := SessionStub{}
	var serverConnection ConnStub
	serverConnection, session.connection = newTwoSidedConn()
	opChan := make(chan *RecordedOp, 1000)

	return &recordedOpGenerator{
		session:          &session,
		serverConnection: &serverConnection,
		opChan:           opChan,
	}

}

//pushDriverRequestOps takes the pair of ops that the driver generator (the nonce and the main op)
// and places them in the channel
func (generator *recordedOpGenerator) pushDriverRequestOps(recordedOp *RecordedOp) {
	generator.opChan <- recordedOp
}

type driverRequestOps struct {
	nonce *RecordedOp
	op    *RecordedOp
}

//TestOpInsertLiveDB tests the functionality of mongotape replaying inserts against a live database
//Generates 20 recorded inserts and passes them to the main execution of mongotape and queries the database
//to verify they were completed. It then checks its BufferedStatCollector to ensure the inserts match what we expected
func TestOpInsertLiveDB(t *testing.T) {
	if err := teardownDB(); err != nil {
		t.Error(err)
	}

	numInserts := 20
	insertName := "LiveDB Insert test"
	generator := newRecordedOpGenerator()
	go func() {
		defer close(generator.opChan)
		//generate numInserts RecordedOps
		t.Logf("Generating %d inserts\n", numInserts)
		err := generator.generateInsertHelper(insertName, 0, numInserts)
		if err != nil {
			t.Error(err)
		}
		t.Log("Generating getLastError")
		err = generator.generateGetLastError()
		if err != nil {
			t.Error(err)
		}
	}()

	statRec := NewBufferedStatRecorder()
	context := NewExecutionContext(statRec)

	//run Mongotape's Play loop with the stubbed objects
	t.Logf("Beginning Mongotape playback of generated traffic against host: %v\n", currentTestServerUrl)
	err := Play(context, generator.opChan, testSpeed, currentTestServerUrl, 1, 10)
	if err != nil {
		t.Errorf("Error Playing traffic: %v\n", err)
	}
	t.Log("Completed Mongotape playback of generated traffic")

	//prepare a query for the database
	session, err := mgo.Dial(currentTestServerUrl)
	if err != nil {
		t.Errorf("Error connecting to test server: %v", err)
	}

	coll := session.DB(testDB).C(testCollection)

	iter := coll.Find(bson.D{}).Sort("docNum").Iter()
	ind := 0
	result := testDoc{}

	//iterate over the results of the query and ensure they match expected documents
	t.Log("Querying database to ensure insert occured successfully")
	for iter.Next(&result) {
		t.Logf("Query result: %#v\n", result)
		if result.DocumentNumber != ind {
			t.Errorf("Inserted document number did not match expected document number. Found: %v -- Expected: %v", result.DocumentNumber, ind)
		}
		if result.Name != insertName {
			t.Errorf("Inserted document name did not match expected name. Found %v -- Expected: %v", result.Name, "LiveDB Insert Test")
		}
		if !result.Success {
			t.Errorf("Inserted document field 'Success' was expected to be true, but was false")
		}
		ind++
	}
	if err := iter.Close(); err != nil {
		t.Error(err)
	}

	//iterate over the operations found by the BufferedStatCollector
	t.Log("Examining collected stats to ensure they match expected")
	for i := 0; i < numInserts; i++ {
		stat := statRec.Buffer[i]
		t.Logf("Stat result: %#v\n", stat)
		//All commands should be inserts into mongotape.test
		if stat.OpType != "insert" ||
			stat.Ns != "mongotape.test" {
			t.Errorf("Expected to see an insert into mongotape.test, but instead saw %v, %v\n", stat.OpType, stat.Command)
		}
	}
	stat := statRec.Buffer[numInserts]
	t.Logf("Stat result: %#v\n", stat)
	if stat.OpType != "command" ||
		stat.Ns != "admin.$cmd" ||
		stat.Command != "getLastError" {
		t.Errorf("Epected to see a get last error, but instead saw %v, %v\n", stat.OpType, stat.Command)
	}
	if err := teardownDB(); err != nil {
		t.Error(err)
	}
}

//TestQueryOpLiveDB tests the functionality of some basic queries through mongotape.
//It generates inserts and queries and sends them to the main execution of mongotape.
//TestQueryOp then examines a BufferedStatCollector to ensure the queries executed as expected
func TestQueryOpLiveDB(t *testing.T) {
	if err := teardownDB(); err != nil {
		t.Error(err)
	}

	insertName := "LiveDB Query Test"
	numInserts := 20
	numQueries := 4
	generator := newRecordedOpGenerator()
	go func() {
		defer close(generator.opChan)
		//generate numInsert inserts and feed them to the opChan for tape
		for i := 0; i < numQueries; i++ {
			t.Logf("Generating %d inserts\n", numInserts/numQueries)
			err := generator.generateInsertHelper(fmt.Sprintf("%s: %d", insertName, i), i*(numInserts/numQueries), numInserts/numQueries)
			if err != nil {
				t.Error(err)
			}
		}
		//generate numQueries queries and feed them to the opChan for play
		t.Logf("Generating %d queries\n", numQueries+1)
		for j := 0; j < numQueries; j++ {
			querySelection := bson.D{{"name", fmt.Sprintf("LiveDB Query Test: %d", j)}}
			err := generator.generateQuery(querySelection, 0, 0)
			if err != nil {
				t.Error(err)
			}
		}
		//generate another query that tests a different field
		querySelection := bson.D{{"success", true}}
		err := generator.generateQuery(querySelection, 0, 0)
		if err != nil {
			t.Error(err)
		}
	}()

	statRec := NewBufferedStatRecorder()
	context := NewExecutionContext(statRec)

	//run Mongotape's Play loop with the stubbed objects
	t.Logf("Beginning Mongotape playback of generated traffic against host: %v\n", currentTestServerUrl)
	err := Play(context, generator.opChan, testSpeed, currentTestServerUrl, 1, 10)
	if err != nil {
		t.Errorf("Error Playing traffic: %v\n", err)
	}
	t.Log("Completed Mongotape playback of generated traffic")

	t.Log("Examining collected stats to ensure they match expected")
	for i := 0; i < numQueries; i++ { //loop over the BufferedStatCollector for each of the numQueries queries created
		stat := statRec.Buffer[numInserts+i]
		t.Logf("Stat result: %#v\n", stat)
		if stat.OpType != "query" ||
			stat.Ns != "mongotape.test" ||
			stat.NumReturned != 5 { //ensure that they match what we expected mongotape to have executed
			t.Errorf("Query Not Matched %#v\n", stat)
		}
	}
	stat := statRec.Buffer[numInserts+numQueries]
	t.Logf("Stat result: %#v\n", stat)
	if stat.OpType != "query" ||
		stat.Ns != "mongotape.test" ||
		stat.NumReturned != 20 { //ensure that the last query that was making a query on the 'success' field executed how we expected
		t.Errorf("Query Not Matched %#v\n", stat)
	}
	if err := teardownDB(); err != nil {
		t.Error(err)
	}

}

//TestOpGetMoreLiveDB tests the functionality of a getmore command played through mongotape.
//It generates inserts, a query, and a series of getmores based on the original query. It then
//Uses a BufferedStatCollector to ensure the getmores executed as expected
func TestOpGetMoreLiveDB(t *testing.T) {
	if err := teardownDB(); err != nil {
		t.Error(err)
	}
	generator := newRecordedOpGenerator()
	insertName := "LiveDB Getmore Test"
	var requestId int32 = 2
	numInserts := 20
	numGetMores := 3
	go func() {
		defer close(generator.opChan)
		t.Logf("Generating %d inserts\n", numInserts)
		//generate numInserts RecordedOp inserts and push them into a channel for use in mongotape's Play()
		err := generator.generateInsertHelper(insertName, 0, numInserts)
		if err != nil {
			t.Error(err)
		}
		querySelection := bson.D{}

		t.Log("Generating query")
		//generate a query with a known requestId to be played in mongotape
		err = generator.generateQuery(querySelection, 5, requestId)
		if err != nil {
			t.Error(err)
		}
		t.Log("Generating reply")
		//generate a RecordedOp reply whose ResponseTo field matches that of the original with a known cursorId
		//so that these pieces of information can be correlated by mongotape
		err = generator.generateReply(requestId, testCursorId, 0)
		if err != nil {
			t.Error(err)
		}
		t.Log("Generating getMore")
		//generate numGetMores RecordedOp getmores with a cursorId matching that of the found reply
		for i := 0; i < numGetMores; i++ {
			err = generator.generateGetMore(testCursorId, 5)
			if err != nil {
				t.Error(err)
			}
		}
	}()
	statRec := NewBufferedStatRecorder()
	context := NewExecutionContext(statRec)

	//run Mongotape's Play loop with the stubbed objects
	t.Logf("Beginning Mongotape playback of generated traffic against host: %v\n", currentTestServerUrl)
	err := Play(context, generator.opChan, testSpeed, currentTestServerUrl, 1, 10)
	if err != nil {
		t.Errorf("Error Playing traffic: %v\n", err)
	}
	t.Log("Completed Mongotape playback of generated traffic")

	//loop over the BufferedStatCollector in the positions the getmores should have been played int
	t.Log("Examining collected stats to ensure they match expected")
	for i := 0; i < numGetMores; i++ {
		stat := statRec.Buffer[numInserts+1+i]
		t.Logf("Stat result: %#v\n", stat)
		if stat.OpType != "getmore" ||
			stat.NumReturned != 5 ||
			stat.Ns != "mongotape.test" { //ensure that each getMore matches the criteria we expected it to have
			t.Errorf("Getmore Not matched: %#v\n", stat)
		}
	}
	if err := teardownDB(); err != nil {
		t.Error(err)
	}
}

//TestOpGetMoreMultiCursorLiveDB tests the functionality of getmores using multiple cursors against a live database.
//It generates a series of inserts followed by two seperate queries. It then uses each of those queries to generate multiple getmores.
//TestOpGetMoreMultiCursorLiveDB uses a BufferedStatCollector to ensure that each getmore played against the database is executed and recieves
//the response expected
func TestOpGetMoreMultiCursorLiveDB(t *testing.T) {
	if err := teardownDB(); err != nil {
		t.Error(err)
	}
	generator := newRecordedOpGenerator()
	var cursor1 int64 = 123
	var cursor2 int64 = 456
	numInserts := 20
	insertName := "LiveDB Multi-Cursor GetMore Test"
	numGetMoresLimit5 := 3
	numGetMoresLimit2 := 9
	go func() {
		defer close(generator.opChan)

		//generate numInserts RecordedOp inserts and send them to a channel to be played in mongotape's main Play function
		t.Logf("Generating %v inserts\n", numInserts)
		err := generator.generateInsertHelper(insertName, 0, numInserts)
		if err != nil {
			t.Error(err)
		}
		querySelection := bson.D{{}}
		var responseId1 int32 = 2
		//generate a first query with a known requestId
		t.Log("Generating query/reply pair")
		err = generator.generateQuery(querySelection, 2, responseId1)
		if err != nil {
			t.Error(err)
		}

		//generate a reply with a known cursorId to be the direct response to the first Query
		err = generator.generateReply(responseId1, cursor1, 0)
		if err != nil {
			t.Error(err)
		}

		var responseId2 int32 = 3
		t.Log("Generating query/reply pair")
		//generate a second query with a different requestId
		err = generator.generateQuery(querySelection, 5, responseId2)
		if err != nil {
			t.Error(err)
		}
		//generate a reply to the second query with another known cursorId
		err = generator.generateReply(responseId2, cursor2, 0)
		if err != nil {
			t.Error(err)
		}
		t.Logf("Generating interleaved getmores")
		//Issue a series of interleaved getmores using the different cursorIds
		for i := 0; i < numGetMoresLimit2; i++ {
			if i < numGetMoresLimit5 {
				//generate a series of getMores using cursorId2 and a limit of 5
				err = generator.generateGetMore(cursor2, 5)
				if err != nil {
					t.Error(err)
				}
			}
			//generate a series of getMores using cursorId2 and a limit of 5
			err = generator.generateGetMore(cursor1, 2)
			if err != nil {
				t.Error(err)
			}
		}
	}()
	statRec := NewBufferedStatRecorder()
	context := NewExecutionContext(statRec)

	//run Mongotape's Play loop with the stubbed objects
	t.Logf("Beginning Mongotape playback of generated traffic against host: %v\n", currentTestServerUrl)
	err := Play(context, generator.opChan, testSpeed, currentTestServerUrl, 1, 10)
	if err != nil {
		t.Errorf("Error Playing traffic: %v\n", err)
	}
	t.Log("Completed Mongotape playback of generated traffic")

	shouldBeLimit5 := true
	var limit int
	totalGetMores := numGetMoresLimit5 + numGetMoresLimit2

	t.Log("Examining collected getMores to ensure they match expected")
	//loop over the total number of getmores played at their expected positions in the BufferedStatCollector
	for i := 0; i < totalGetMores; i++ {
		stat := statRec.Buffer[numInserts+2+i]
		t.Logf("Stat result: %#v\n", stat)
		//the first set of getmores should be alternating between having a limit of 5 and a limit of 2
		if i < numGetMoresLimit5*2 && shouldBeLimit5 {
			limit = 5
			shouldBeLimit5 = !shouldBeLimit5
		} else { //after seeing all those with a limit of 5 we should see exclusively getmores with limit 2
			shouldBeLimit5 = !shouldBeLimit5
			limit = 2
		}
		if stat.OpType != "getmore" ||
			stat.NumReturned != limit ||
			stat.Ns != "mongotape.test" { //ensure that the operations in the BufferedStatCollector match what expected
			t.Errorf("Getmore Not matched: %#v\n", stat)
		}
	}
	if err := teardownDB(); err != nil {
		t.Error(err)
	}
}

func teardownDB() error {
	session, err := mgo.Dial(currentTestServerUrl)
	if err != nil {
		return err
	}

	session.DB(testDB).C(testCollection).DropCollection()
	session.Close()
	return nil
}

//generateInsert creates a RecordedOp insert using the given documents and pushes it to the recordedOpGenerator's channel
//to be executed when Play() is called
func (generator *recordedOpGenerator) generateInsert(docs []interface{}) error {
	insert := mgo.InsertOp{Collection: fmt.Sprintf("%s.%s", testDB, testCollection),
		Documents: docs,
		Flags:     0,
	}
	recordedOp, err := generator.fetchRecordedOpsFromConn(&insert)
	if err != nil {
		return err
	}
	generator.pushDriverRequestOps(recordedOp)
	return nil

}

func (generator *recordedOpGenerator) generateGetLastError() error {
	getLastError := mgo.QueryOp{
		Collection: "admin.$cmd",
		Query:      bson.D{{Name: "getLastError", Value: 1}},
		Limit:      -1,
		Flags:      0,
	}
	recordedOp, err := generator.fetchRecordedOpsFromConn(&getLastError)
	if err != nil {
		return err
	}
	generator.pushDriverRequestOps(recordedOp)
	return nil

}

func (generator *recordedOpGenerator) generateInsertHelper(name string, startFrom, numInserts int) error {
	for i := 0; i < numInserts; i++ {
		doc := testDoc{
			Name:           name,
			DocumentNumber: i + startFrom,
			Success:        true,
		}
		err := generator.generateInsert([]interface{}{doc})
		if err != nil {
			return err
		}
	}
	return nil
}

//generateQuery creates a RecordedOp query using the given selector, limit, and requestId, and pushes it to the recordedOpGenerator's channel
//to be executed when Play() is called
func (generator *recordedOpGenerator) generateQuery(querySelection interface{}, limit int32, requestId int32) error {
	query := mgo.QueryOp{
		Flags:      0,
		HasOptions: true,
		Skip:       0,
		Limit:      limit,
		Selector:   bson.D{},
		Query:      querySelection,
		Collection: fmt.Sprintf("%s.%s", testDB, testCollection),
		Options:    mgo.QueryWrapper{},
	}

	recordedOp, err := generator.fetchRecordedOpsFromConn(&query)
	if err != nil {
		return err
	}
	recordedOp.RawOp.Header.RequestID = requestId
	generator.pushDriverRequestOps(recordedOp)
	return nil
}

//generateGetMore creates a RecordedOp getMore using the given cursorId and limit and pushes it to the recordedOpGenerator's channel
//to be executed when Play() is called
func (generator *recordedOpGenerator) generateGetMore(cursorId int64, limit int32) error {
	getMore := mgo.GetMoreOp{
		Collection: fmt.Sprintf("%s.%s", testDB, testCollection),
		CursorId:   cursorId,
		Limit:      limit,
	}

	recordedOp, err := generator.fetchRecordedOpsFromConn(&getMore)
	if err != nil {
		return err
	}
	generator.pushDriverRequestOps(recordedOp)
	return nil
}

//generateReply creates a RecordedOp reply using the given responseTo, cursorId, and firstDOc, and pushes it to the recordedOpGenerator's channel
//to be executed when Play() is called
func (generator *recordedOpGenerator) generateReply(responseTo int32, cursorId int64, firstDoc int32) error {
	reply := mgo.ReplyOp{
		Flags:     0,
		CursorId:  cursorId,
		FirstDoc:  firstDoc,
		ReplyDocs: 5,
	}

	recordedOp, err := generator.fetchRecordedOpsFromConn(&reply)
	if err != nil {
		return err
	}

	recordedOp.RawOp.Header.ResponseTo = responseTo
	mongoproto.SetInt64(recordedOp.RawOp.Body, 4, cursorId) //change the cursorId field in the RawOp.Body
	tempEnd := recordedOp.SrcEndpoint
	recordedOp.SrcEndpoint = recordedOp.DstEndpoint
	recordedOp.DstEndpoint = tempEnd
	generator.pushDriverRequestOps(recordedOp)
	return nil
}

//fetchRecordedOpsFromConn runs the created mgo op through mgo and fetches its result from the stubbed connection.
//In the case that a connection has not been used before it reads two ops from the connection, the first being the
//'getNonce' request generated by the driver
func (generator *recordedOpGenerator) fetchRecordedOpsFromConn(op interface{}) (*RecordedOp, error) {
	socket, err := generator.session.AcquireSocketPrivate(true)
	if err != nil {
		return nil, fmt.Errorf("AcquireSocketPrivate: %v\n", err)
	}
	err = socket.Query(op)
	if err != nil {
		return nil, fmt.Errorf("Socket.Query: %v\n", err)
	}
	msg, err := mongoproto.ReadHeader(generator.serverConnection)
	if err != nil {
		return nil, fmt.Errorf("ReadHeader Error: %v\n", err)
	}
	result := mongoproto.RawOp{Header: *msg}
	result.Body = make([]byte, mongoproto.MsgHeaderLen)
	result.FromReader(generator.serverConnection)

	recordedOp := &RecordedOp{RawOp: result, Seen: testTime, SrcEndpoint: "a", DstEndpoint: "b"}

	d, _ := time.ParseDuration("2ms")
	testTime = testTime.Add(d)
	return recordedOp, nil
}
