import { describe, expect, it } from 'vitest'
import { type Anchor, anchor, isComment, lines, prompt, where } from './patch'

describe('lines', () => {
  it('numbers the lines of each hunk on both sides', () => {
    const patch = [
      'diff --git a/parse.go b/parse.go',
      'index 1..2 100644',
      '--- a/parse.go',
      '+++ b/parse.go',
      '@@ -1,3 +1,4 @@ package parse',
      ' package parse',
      '-func Old() {}',
      '+func Parse() {}',
      '+func More() {}',
      ' // end',
      '\\ No newline at end of file',
      '@@ -20 +21 @@',
      '-x',
      '+y',
      '',
    ].join('\n')
    expect(lines(patch).map((l) => [l.kind, l.old, l.new, l.text])).toEqual([
      ['meta', undefined, undefined, 'diff --git a/parse.go b/parse.go'],
      ['meta', undefined, undefined, 'index 1..2 100644'],
      ['meta', undefined, undefined, '--- a/parse.go'],
      ['meta', undefined, undefined, '+++ b/parse.go'],
      ['hunk', undefined, undefined, '@@ -1,3 +1,4 @@ package parse'],
      ['context', 1, 1, 'package parse'],
      ['removed', 2, undefined, 'func Old() {}'],
      ['added', undefined, 2, 'func Parse() {}'],
      ['added', undefined, 3, 'func More() {}'],
      ['context', 3, 4, '// end'],
      ['note', undefined, undefined, '\\ No newline at end of file'],
      ['hunk', undefined, undefined, '@@ -20 +21 @@'],
      ['removed', 20, undefined, 'x'],
      ['added', undefined, 21, 'y'],
    ])
  })
  it('keeps what precedes a hunk, and a cut patch, as meta', () => {
    expect(lines('Binary files a/logo.png and b/logo.png differ').map((l) => l.kind)).toEqual(['meta'])
    expect(lines('@@ -1 +1 @@\n-a\n+b\n\n[cut at 1 MiB]').map((l) => l.kind)).toEqual([
      'hunk',
      'removed',
      'added',
      'meta',
      'meta',
    ])
  })
})

// at anchors the row of a patch whose text is text, the nth such row.
function at(patch: string, text: string, nth = 0): Anchor {
  const rows = lines(patch)
  const i = rows.map((l, j) => (l.text === text ? j : -1)).filter((j) => j >= 0)[nth] ?? -1
  const a = anchor(rows, i)
  if (!a) throw new Error(`no line ${text}`)
  return a
}

// found is the text and new number of the row an anchor is found on.
function found(patch: string, a: Anchor) {
  const rows = lines(patch)
  const i = where(rows, a)
  return i < 0 ? undefined : { text: rows[i]?.text, line: anchor(rows, i)?.line }
}

describe('where', () => {
  const a = '@@ -1,3 +1,3 @@\n function a() {\n-  return 1;\n+  return null;\n }\n'
  it('follows a line as lines come before it', () => {
    const x = at('@@ -1,2 +1,3 @@\n a\n+x\n b\n', 'x')
    expect(found('@@ -1,2 +1,4 @@\n a\n+new\n+x\n b\n', x)).toEqual({ text: 'x', line: 3 })
  })
  it('tells a line from another like it that came before it, by the lines around it', () => {
    const before =
      '@@ -1,3 +1,6 @@\n+function b() {\n+  return null;\n+}\n function a() {\n-  return 1;\n+  return null;\n }\n'
    expect(found(before, at(a, '  return null;'))).toEqual({ text: '  return null;', line: 5 })
  })
  it('finds no line that is gone, though another like it remains', () => {
    const gone = '@@ -1,3 +1,3 @@\n+function b() {\n+  return null;\n+}\n-function a() {\n-  return 1;\n-}\n'
    expect(found(gone, at(a, '  return null;'))).toBeUndefined()
  })
  it('counts each line around once, so that one blank line does not stand for three', () => {
    const b = ['beginB();', 'prepareB();', '', 'run();', 'finishB();', 'closeB();', 'endB();']
    const a = ['', '', '', 'run();', 'finishA();', 'closeA();', 'endA();']
    const both = `@@ -0,0 +1,14 @@\n${[...b, ...a].map((l) => `+${l}`).join('\n')}\n`
    const onlyB = `@@ -0,0 +1,7 @@\n${b.map((l) => `+${l}`).join('\n')}\n`
    expect(found(onlyB, at(both, 'run();', 1))).toBeUndefined()
  })
  it('finds no line it cannot tell apart from another', () => {
    expect(found('@@ -0,0 +1,2 @@\n+x\n+x\n', at('@@ -0,0 +1 @@\n+x\n', 'x'))).toBeUndefined()
  })
  it('tells a removed line from an added one with its text', () => {
    const removed = at(a, '  return 1;')
    expect(removed.removed).toBe(true)
    expect(found(a, removed)).toEqual({ text: '  return 1;', line: 2 })
  })
})

describe('prompt', () => {
  it('names the file and line of each comment, numbered as its patch has it now, and quotes the line', () => {
    const then = '@@ -1 +1,3 @@\n package parse\n+\n+func Parse() {}\n'
    const now = lines('@@ -1 +1,4 @@\n package parse\n+\n+// Parse parses.\n+func Parse() {}\n')
    const gone = { ...at(then, 'package parse'), file: 'gone.go', text: 'unread file' }
    expect(
      prompt(
        [
          { ...at(then, 'func Parse() {}'), file: 'parse.go', text: ' name it Read ' },
          { ...at('@@ -1,2 +1 @@\n a\n-old()\n', 'old()'), file: 'parse.go', text: 'why remove this?' },
          gone,
        ],
        (file) => (file === 'parse.go' ? now : []),
      ),
    ).toBe(
      [
        'parse.go:4: name it Read',
        '> func Parse() {}',
        'parse.go:2 (removed): why remove this?',
        '> old()',
        'gone.go:1: unread file',
        '> package parse',
      ].join('\n'),
    )
  })
})

describe('isComment', () => {
  it('takes back only comments from what the tab kept', () => {
    const ok = { ...at('@@ -1 +1,2 @@\n a\n+b\n', 'b'), file: 'parse.go', text: 'name it Read' }
    const kept: unknown[] = [
      ok,
      { ...ok, line: '3' },
      { ...ok, before: ['a'] },
      { ...ok, hash: undefined },
      null,
      'parse.go:3',
    ]
    expect(kept.filter(isComment)).toEqual([ok])
  })
})
