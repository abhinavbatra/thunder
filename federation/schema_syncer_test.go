package federation

import (
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/samsarahq/go/oops"
	"github.com/samsarahq/thunder/graphql/introspection"
	"github.com/samsarahq/thunder/graphql/schemabuilder"
	"github.com/stretchr/testify/require"
)

type FileSchemaSyncer struct {
	services        []string
	add             chan string // new URL channel
	currentSchema   []byte
	serviceSelector ServiceSelector
}

func newFileSchemaSyncer(ctx context.Context, services []string) *FileSchemaSyncer {
	ss := &FileSchemaSyncer{
		services: services,
		add:      make(chan string),
	}
	return ss
}

func (s *FileSchemaSyncer) FetchPlanner(ctx context.Context) (*Planner, error) {
	schemas := make(map[string]*introspectionQueryResult)
	for _, server := range s.services {
		schema, err := readFile(server)
		if err != nil {
			return nil, oops.Wrapf(err, "error reading file for server %s", server)
		}
		var iq introspectionQueryResult
		if err := json.Unmarshal(schema, &iq); err != nil {
			return nil, oops.Wrapf(err, "unmarshaling schema %s", server)
		}
		schemas[server] = &iq
	}

	types, err := convertSchema(schemas)
	if err != nil {
		return nil, oops.Wrapf(err, "converting schemas error")
	}

	introspectionSchema := introspection.BareIntrospectionSchema(types.Schema)
	schema, err := introspection.RunIntrospectionQuery(introspection.BareIntrospectionSchema(introspectionSchema))
	if err != nil || schema == nil {
		return nil, oops.Wrapf(err, "error running introspection query")
	}

	var iq introspectionQueryResult
	if err := json.Unmarshal(schema, &iq); err != nil {
		return nil, oops.Wrapf(err, "unmarshaling introspection schema")
	}

	schemas["introspection"] = &iq
	types, err = convertSchema(schemas)
	if err != nil {
		return nil, oops.Wrapf(err, "converting schemas error")
	}

	return NewPlanner(types, s.serviceSelector)
}

// WriteToFile will print any string of text to a file safely by
// checking for errors and syncing at the end.
func WriteToFile(filename string, data string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.WriteString(file, data)
	if err != nil {
		return err
	}
	return file.Sync()
}

func writeSchemaToFile(name string, data []byte) error {
	fileName := "./testdata/fileschemasyncer" + name
	file, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.WriteString(file, string(data))
	if err != nil {
		return err
	}
	return file.Sync()
}

func readFile(name string) ([]byte, error) {
	fileName := "./testdata/fileschemasyncer" + name
	return ioutil.ReadFile(fileName)
}

func TestExecutorQueriesWithCustomSchemaSyncer(t *testing.T) {
	s1 := buildTestSchema1()
	s2 := buildTestSchema2()

	ctx := context.Background()
	execs, err := makeExecutors(map[string]*schemabuilder.Schema{
		"s1": s1,
		"s2": s2,
	})
	require.NoError(t, err)

	// Write the schemas to a file
	services := []string{"s1", "s2"}
	for _, service := range services {
		schema, err := fetchSchema(ctx, execs[service], nil)
		require.NoError(t, err)
		err = writeSchemaToFile(service, schema.Result)
		require.NoError(t, err)
	}

	// Creata file schema syncer that reads the schemas from the
	// written files and listens to updates if those change
	schemaSyncer := newFileSchemaSyncer(ctx, services)
	e, err := NewExecutor(ctx, execs, &CustomExecutorArgs{
		SchemaSyncer:              schemaSyncer,
		SchemaSyncIntervalSeconds: func(ctx context.Context) int64 { return 1 },
	})
	require.NoError(t, err)

	// Test Case 1.
	query := `query Foo {
					s2root
					s1fff {
						s1hmm
					}
				}`
	expectedOutput := `{
					"s2root": "hello",
					"s1fff":[
						{
							"s1hmm":"jimbo!!!"
						},
						{
							"s1hmm":"bob!!!"
						}
					]
				}`

	// Run a federated query and ensure that it works
	runAndValidateQueryResults(t, ctx, e, query, expectedOutput)
	time.Sleep(2 * time.Second)

	// Test Case 2.
	// Add a new field to schema2
	s2.Query().FieldFunc("syncerTest", func() string {
		return "hello"
	})

	newExecs, err := makeExecutors(map[string]*schemabuilder.Schema{
		"s1": s1,
		"s2": s2,
	})
	require.NoError(t, err)

	// We need to do this to udpate the executor in our test
	// But when run locally it should already know about the new
	// field when the new service starts
	e.Executors = newExecs

	query2 := `query Foo {
		syncerTest
	}`
	expectedOutput2 := `{
		"syncerTest":"hello"
	}`

	// Since we havent written the new schema to the file, the merged schema and planner
	// dont know about the new field. We should see an error
	runAndValidateQueryError(t, ctx, e, query2, expectedOutput2, "unknown field syncerTest on typ Query")

	// Test case 3.
	// Writes the new schemas to the file
	for _, service := range services {
		schema, err := fetchSchema(ctx, newExecs[service], nil)
		require.NoError(t, err)
		err = writeSchemaToFile(service, schema.Result)
		require.NoError(t, err)
	}

	// Sleep for 2 seconds to wait for the schema syncer to get the update
	time.Sleep(2 * time.Second)

	// 	Run the same query and validate that it works
	runAndValidateQueryResults(t, ctx, e, query2, expectedOutput2)

	// Test case 4.
	// Update the serviceSelector, syncerTestFunc to be resolved by service 1, which
	// should fail the query since service 1 does not know how to resolve it.
	schemaSyncer.serviceSelector = func(typeName string, fieldName string) string {
		if typeName == "Query" && fieldName == "syncerTest" {
			return "s1"
		}
		return ""
	}
	// Sleep for 2 seconds to wait for the schema syncer to get the update
	time.Sleep(2 * time.Second)
	runAndValidateQueryError(t, ctx, e, query2, expectedOutput2, "unknown field \"syncerTest\"")

	// Test case 5.
	// Add the same fieldfunc to s1.
	s1.Query().FieldFunc("syncerTest", func() string {
		return "hello from s1"
	})
	newExecs, err = makeExecutors(map[string]*schemabuilder.Schema{
		"s1": s1,
		"s2": s2,
	})
	require.NoError(t, err)
	// We need to do this to udpate the executor in our test
	// But when run locally it should already know about the new
	// field when the new service starts
	e.Executors = newExecs
	expectedOutput2 = `{
		"syncerTest":"hello from s1"
	}`
	// Sleep for 2 seconds to wait for the schema syncer to get the update
	time.Sleep(2 * time.Second)
	// Run the same query and validate that it works
	runAndValidateQueryResults(t, ctx, e, query2, expectedOutput2)
}
