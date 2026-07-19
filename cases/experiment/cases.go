package experiment

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

// All returns opt-in/low-frequency OpenViking API cases.
func All() []runner.Case {
	return []runner.Case{
		exportImportCase(),
		reindexSemanticCase(),
		addResourceFolderCase(),
		writeRefreshCase(),
		relationsLinkCase(),
	}
}

func exportImportCase() runner.Case {
	return runner.Case{
		ID:        "experiment-ov-export-import",
		Goal:      "Export a resource subtree to .ovpack, import it elsewhere, and verify restored content/retrieval.",
		Reference: "Deterministic gates verify export/import completed and restored content is greppable under the import target.",
		Build: func(b *dag.Builder) {
			tok := "exp" + nonce(3)
			src := "viking://resources/ovtest-experiment/export-" + tok + "-src"
			dst := "viking://resources/ovtest-experiment/export-" + tok + "-dst"
			pack := tempPack(tok)
			dir := fixtureDir(tok, map[string]string{
				"archive.md": fmt.Sprintf("OVTEST %s export import amber circuit archive.\n", tok),
			})
			user := runner.ConfiguredUser(b, "user")
			add := ovCmd(b, "seed", user, nil, []string{"add-resource", dir,
				"--parent-auto-create", src, "--wait", "--timeout", "120"},
				dag.Cfg{"expect": []string{"success", tok}})
			export := ovCmd(b, "export", user, add, []string{"export", src, pack, "--include-vectors"},
				dag.Cfg{"expect": []string{"successfully exported"}})
			imp := ovCmd(b, "import", user, export, []string{"import", pack, dst,
				"--on-conflict", "fail", "--vector-mode", "recompute"},
				dag.Cfg{"expect": []string{tok}})
			wait := ovCmd(b, "wait", user, imp, []string{"wait", "--timeout", "120"}, nil)
			grep := ovCmd(b, "grep_import", user, wait, []string{"grep", "amber circuit", "-u", dst, "-i"},
				dag.Cfg{"expect": []string{tok, "amber circuit"}, "min_count": 1})
			check(b, "export/import roundtrip restored greppable content", add, export, imp, wait, grep)
		},
	}
}

func reindexSemanticCase() runner.Case {
	return runner.Case{
		ID:        "experiment-ov-reindex-semantic",
		Goal:      "Run semantic_and_vectors reindex on a resource subtree and verify content remains searchable.",
		Reference: "The reindex command uses --mode semantic_and_vectors; post-reindex grep still surfaces the seeded resource fact.",
		Build: func(b *dag.Builder) {
			tok := "exp" + nonce(3)
			root := "viking://resources/ovtest-experiment/reindex-" + tok
			uri := root + "/profile.md"
			user := runner.ConfiguredUser(b, "user")
			write := ovCmd(b, "write", user, nil, []string{"write", uri,
				"--mode", "create", "--content", fmt.Sprintf("OVTEST %s semantic reindex heliotrope ledger.", tok),
				"--wait", "--timeout", "120"},
				dag.Cfg{"expect": []string{"semantic_status", "complete", tok}})
			before := ovCmd(b, "grep_before", user, write, []string{"grep", "heliotrope ledger", "-u", root, "-i"},
				dag.Cfg{"expect": []string{tok, "heliotrope ledger"}, "min_count": 1})
			reindex := ovCmd(b, "reindex", user, before, []string{"reindex", root,
				"--mode", "semantic_and_vectors", "--wait", "true"},
				dag.Cfg{"expect": []string{"semantic_and_vectors"}})
			afterGrep := ovCmd(b, "grep_after", user, reindex, []string{"grep", "heliotrope ledger", "-u", root, "-i"},
				dag.Cfg{"expect": []string{tok, "heliotrope ledger"}, "min_count": 1})
			check(b, "semantic_and_vectors reindex completed and grep still works", write, before, reindex, afterGrep)
		},
	}
}

func addResourceFolderCase() runner.Case {
	return runner.Case{
		ID:        "experiment-ov-add-resource-folder",
		Goal:      "Import a local folder with include/exclude filters and verify tree, glob, and grep.",
		Reference: "Only included files are indexed; excluded files do not leak into search evidence.",
		Build: func(b *dag.Builder) {
			tok := "exp" + nonce(3)
			root := "viking://resources/ovtest-experiment/add-resource-" + tok
			dir := fixtureDir(tok, map[string]string{
				"ledger.md": fmt.Sprintf("OVTEST %s add resource violet quantum ledger.\n", tok),
				"skip.tmp":  fmt.Sprintf("OVTEST %s discard token should stay excluded.\n", tok),
			})
			user := runner.ConfiguredUser(b, "user")
			add := ovCmd(b, "add_resource", user, nil, []string{"add-resource", dir,
				"--parent-auto-create", root, "--wait", "--timeout", "120",
				"--include", "*.md,*.txt", "--exclude", "*.tmp"},
				dag.Cfg{"expect": []string{"success", "ledger.md", "skip.tmp"}})
			glob := ovCmd(b, "glob", user, add, []string{"glob", "**/*.md", "-u", root},
				dag.Cfg{"expect": []string{"ledger.md"}, "min_count": 1, "forbid": []string{"skip.tmp"}})
			grep := ovCmd(b, "grep", user, add, []string{"grep", "violet quantum", "-u", root, "-i"},
				dag.Cfg{"expect": []string{tok, "violet quantum"}, "min_count": 1, "forbid": []string{"discard token"}})
			check(b, "add-resource folder import is browsable, greppable, and respects filters", add, glob, grep)
		},
	}
}

func writeRefreshCase() runner.Case {
	return runner.Case{
		ID:        "experiment-ov-write-refresh",
		Goal:      "Exercise write create/append/replace and verify read/grep reflect the latest content.",
		Reference: "Reads and grep show appended content, then replacement removes the old tokens.",
		Build: func(b *dag.Builder) {
			tok := "exp" + nonce(3)
			root := "viking://resources/ovtest-experiment/write-" + tok
			uri := root + "/profile.md"
			user := runner.ConfiguredUser(b, "user")
			create := ovCmd(b, "write_create", user, nil, []string{"write", uri,
				"--mode", "create", "--content", fmt.Sprintf("OVTEST %s original cobalt digest.", tok),
				"--wait", "--timeout", "120"},
				dag.Cfg{"expect": []string{tok, "content_updated"}})
			appendStep := ovCmd(b, "write_append", user, create, []string{"write", uri,
				"--append", "--content", fmt.Sprintf("\nOVTEST %s appended silver digest.", tok),
				"--wait", "--timeout", "120"},
				dag.Cfg{"expect": []string{tok, "content_updated"}})
			grepAppend := ovCmd(b, "grep_append", user, appendStep, []string{"grep", "silver digest", "-u", root, "-i"},
				dag.Cfg{"expect": []string{tok, "silver digest"}, "min_count": 1})
			replace := ovCmd(b, "write_replace", user, grepAppend, []string{"write", uri,
				"--mode", "replace", "--content", fmt.Sprintf("OVTEST %s replacement emerald digest.", tok),
				"--wait", "--timeout", "120"},
				dag.Cfg{"expect": []string{tok, "content_updated"}})
			read := ovCmd(b, "read_replace", user, replace, []string{"read", uri},
				dag.Cfg{"expect": []string{tok, "replacement emerald"}, "forbid": []string{"original cobalt", "appended silver"}})
			grepReplace := ovCmd(b, "grep_replace", user, replace, []string{"grep", "replacement emerald", "-u", root, "-i"},
				dag.Cfg{"expect": []string{tok, "replacement emerald"}, "min_count": 1})
			check(b, "write append/replace evidence reflects current content", create, appendStep, grepAppend, replace, read, grepReplace)
		},
	}
}

func relationsLinkCase() runner.Case {
	return runner.Case{
		ID:        "experiment-ov-relations-link",
		Goal:      "Create, read, and remove a relation link between two resource directories.",
		Reference: "relations shows the target after link and no target after unlink.",
		Build: func(b *dag.Builder) {
			tok := "exp" + nonce(3)
			root := "viking://resources/ovtest-experiment/relations-" + tok
			source := root + "/source"
			target := root + "/target"
			user := runner.ConfiguredUser(b, "user")
			writeSource := ovCmd(b, "write_source", user, nil, []string{"write", source + "/source.md",
				"--mode", "create", "--content", fmt.Sprintf("OVTEST %s relation source.", tok),
				"--wait", "--timeout", "120"}, dag.Cfg{"expect": []string{tok}})
			writeTarget := ovCmd(b, "write_target", user, writeSource, []string{"write", target + "/target.md",
				"--mode", "create", "--content", fmt.Sprintf("OVTEST %s relation target.", tok),
				"--wait", "--timeout", "120"}, dag.Cfg{"expect": []string{tok}})
			link := ovCmd(b, "link", user, writeTarget, []string{"link", source, target, "--reason", "ovtest relation probe"},
				dag.Cfg{"expect": []string{source, target}})
			relations := ovCmd(b, "relations_before", user, link, []string{"relations", source},
				dag.Cfg{"expect": []string{target, "ovtest relation probe"}, "min_count": 1})
			unlink := ovCmd(b, "unlink", user, relations, []string{"unlink", source, target},
				dag.Cfg{"expect": []string{source, target}})
			relationsAfter := ovCmd(b, "relations_after", user, unlink, []string{"relations", source},
				dag.Cfg{"forbid": []string{target}})
			check(b, "relations link/unlink lifecycle is reflected by relations", writeSource, writeTarget, link, relations, unlink, relationsAfter)
		},
	}
}

func ovCmd(b *dag.Builder, name string, user *dag.Node, after dag.Input, args []string, cfg dag.Cfg) *dag.Node {
	if cfg == nil {
		cfg = dag.Cfg{}
	}
	cfg["args"] = args
	in := dag.In{"user_key": user}
	if after != nil {
		in["after"] = after
	}
	return b.Add(ovops.Command, dag.Spec{Name: name, In: in, Config: cfg})
}

func check(b *dag.Builder, explanation string, nodes ...*dag.Node) {
	b.Add(checks.Deterministic, dag.Spec{Name: "check",
		In:     dag.In{"after": runner.FanIn(b, nodes...)},
		Config: dag.Cfg{"explanation": explanation}})
}

func fixtureDir(token string, files map[string]string) string {
	dir, err := os.MkdirTemp("", "ovtest-experiment-"+token+"-*")
	if err != nil {
		panic(err)
	}
	// ponytail: temp fixtures are tiny; rely on OS temp cleanup unless repeat volume matters.
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			panic(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			panic(err)
		}
	}
	return dir
}

func tempPack(token string) string {
	dir, err := os.MkdirTemp("", "ovtest-pack-"+token+"-*")
	if err != nil {
		panic(err)
	}
	return filepath.Join(dir, "bundle.ovpack")
}

func nonce(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
