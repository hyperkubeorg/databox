//go:build e2e

// gateway_vector_test.go — TestGatewayVectorKNN: the §13 vector extension
// end to end — VECTOR(n) DDL, vector literals through both the simple and
// extended (parameterized) protocols, exact-KNN ordering via the <->
// operator, and a distance value computed by l2_distance — all over a real
// pg wire v3 conversation against the in-process SQL gateway backed by a
// real cluster. Reuses the hand-rolled pg frontend from
// gateway_sql_test.go.
package e2e

import (
	"testing"
)

// TestGatewayVectorKNN — GUARANTEE: the SQL layer's pgvector-style surface
// works over the wire: VECTOR(3) columns store and return canonical
// '[...]' text, $N text parameters carry vectors through the extended
// protocol, and ORDER BY embedding <-> $query LIMIT k returns the exact
// nearest neighbors (no index, single-pass top-k scan).
func TestGatewayVectorKNN(t *testing.T) {
	nodes := startCluster(t, 1)
	addr := startSQLGateway(t, nodes[0].endpoint())

	pg := dialPG(t, addr, "root", "", "e2edb")

	if rows := pg.query("CREATE TABLE items (id INTEGER PRIMARY KEY, embedding VECTOR(3))"); len(rows) != 0 {
		t.Fatalf("CREATE TABLE returned rows: %v", rows)
	}

	// A handful of vectors: some through the simple protocol...
	pg.query("INSERT INTO items VALUES (1, '[0, 0, 0]'), (2, '[1, 1, 1]')")
	// ...and some through the extended protocol with $N text parameters.
	pg.execExtended("INSERT INTO items (id, embedding) VALUES ($1, $2)", "3", "[2,2,2]")
	pg.execExtended("INSERT INTO items (id, embedding) VALUES ($1, $2)", "4", "[10,10,10]")
	pg.execExtended("INSERT INTO items (id, embedding) VALUES ($1, $2)", "5", "[-1,-1,-1]")

	// Stored vectors come back in canonical text form.
	rows := pg.query("SELECT embedding FROM items WHERE id = 1")
	if len(rows) != 1 || rows[0][0] != "[0,0,0]" {
		t.Fatalf("embedding text = %v, want [[0,0,0]]", rows)
	}

	// Exact KNN: nearest two to [3,3,3] are id 3 ([2,2,2]) then id 2
	// ([1,1,1]).
	rows = pg.query("SELECT id FROM items ORDER BY embedding <-> '[3,3,3]' LIMIT 2")
	if len(rows) != 2 || rows[0][0] != "3" || rows[1][0] != "2" {
		t.Fatalf("KNN rows = %v, want [[3] [2]]", rows)
	}

	// The distance itself: |[0,0,0] - [3,4,0]| = 5.
	rows = pg.query("SELECT l2_distance(embedding, '[3,4,0]') FROM items WHERE id = 1")
	if len(rows) != 1 || rows[0][0] != "5" {
		t.Fatalf("l2_distance = %v, want [[5]]", rows)
	}

	// The distance operator composes with WHERE like any double: only ids
	// 2 (dist √12) and 3 (dist √3) lie within distance 4 of [3,3,3].
	rows = pg.query("SELECT id FROM items WHERE embedding <-> '[3,3,3]' < 4 ORDER BY id")
	if len(rows) != 2 || rows[0][0] != "2" || rows[1][0] != "3" {
		t.Fatalf("range-filtered rows = %v, want [[2] [3]]", rows)
	}
}
