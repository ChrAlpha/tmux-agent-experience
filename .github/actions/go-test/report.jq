# The failures among go test's JSON events, read as raw lines, as workflow
# commands that annotate them ($format == "annotations") or as Markdown for
# the job summary ($format == "summary"). Only the deepest failures count: a
# test that failed because its subtests did, or a package because its tests
# did, adds nothing. Package paths start with the module's path, $root.

def escape: gsub("%"; "%25") | gsub("\r"; "%0D") | gsub("\n"; "%0A");
def property: escape | gsub(":"; "%3A") | gsub(","; "%2C");
def directory: ltrimstr($root) | ltrimstr("/");

[inputs | fromjson? | select(.Package or .ImportPath)] as $events
| [$events[] | select(.Action == "fail")] as $fails
| [ $fails[] | . as $f
    | select(any($fails[]; .Package == $f.Package and .Test != $f.Test
        and ($f.Test == null or ((.Test // "") | startswith($f.Test + "/")))) | not)
    | { package: .Package, test: .Test,
        output: ([ $events[]
          | select((.Action == "output" and .Package == $f.Package and .Test == $f.Test)
            or ($f.FailedBuild != null and .Action == "build-output" and .ImportPath == $f.FailedBuild))
          | .Output ] | add // "") }
  ] | unique_by([.package, .test]) as $failures
| if $format == "annotations" then
    $failures[]
    | (.test // (.package | directory)) as $title
    | (.package | directory | if . == "" then . else . + "/" end) as $dir
    # A test's messages name a file in its package's directory; the
    # compiler's and vet's name one from the module's.
    | [ .output | splits("\n")
        | (capture("^\\s+(?<file>[^\\s:]+_test\\.go):(?<line>[0-9]+): (?<message>.*)") | .file = $dir + .file)
          // capture("^(vet: )?(?<file>[^\\s:]+\\.go):(?<line>[0-9]+):([0-9]+:)? (?<message>.*)") ] as $messages
    | if $messages == [] then
        "::error title=\($title | property)::\($title) failed; its output is in the job summary"
      else
        limit(3; $messages[])
        | "::error file=\(.file | property),line=\(.line),title=\($title | property)::\(.message | escape)"
      end
  elif $failures != [] then
    "**\($failures | length) failed**\n",
    ( $failures[]
      | "<details><summary><code>\(.test // "the package")</code> in <code>\(.package | directory)</code></summary>\n",
        "````",
        (.output | rtrimstr("\n") | split("\n") | .[-150:] | join("\n")),
        "````\n\n</details>\n" )
  else empty
  end
