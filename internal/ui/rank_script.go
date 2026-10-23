package ui

// rankJS is the browser's copy of internal/searchrank — window.musterFold and
// window.musterRank — spelled ONCE and embedded in every document that ranks:
// the app shell (appScript: the pickers, the Agents filter) and the mirrored
// Rendered view's `/` and `:` menu (composerMenuScript). The Rendered view
// carries appScript too (overlayNavScript, for its side nav), so it holds two
// byte-identical copies; the menu embeds its own anyway, so it never depends
// on another script having run (TestBothDocumentsRankWithOneFunction).
//
// 🔴 ONE SOURCE, BECAUSE TWO COPIES DRIFT. e2e/tests/pickers.spec.ts holds this
// function to internal/searchrank/testdata/cases.json on the app shell, and
// e2e/tests/composer-menu.spec.ts holds it to the same table on the mirrored
// view — two documents, one string.
//
// It assigns the two globals (idempotently) and declares nothing else, so it
// can sit inside any IIFE.
const rankJS = `  // window.musterRank is EVERY search picker's rank — 0 the label starts with
  // the query, 1 a word in it does, 2 it is anywhere in it, 3 its letters are
  // in it in order, -1 no match — the browser's copy of internal/searchrank.
  // ONE table holds both copies to the same answers: internal/searchrank/
  // testdata/cases.json, read by the Go test and run through this function by
  // e2e/tests/pickers.spec.ts. Tier 3 is the letters-in-order match these
  // pickers always had, kept LAST (searchrank's doc says why it stays).
  // window.musterFold is searchrank.Fold, step for step: NFKD, combining marks
  // and U+FEFF dropped, lower case, final sigma as sigma, white space trimmed
  // (U+0085 included, as Go's unicode.IsSpace does). Code points, never UTF-16
  // units: a letter outside the BMP is one letter here as it is in Go.
  var RANK_MARK = (function () { try { return new RegExp('\\p{M}', 'gu'); } catch (e) { return /[\u0300-\u036f]/g; } })();
  var RANK_WORD = (function () { try { return new RegExp('^[\\p{L}\\p{N}]$', 'u'); } catch (e) { return /^[a-z0-9\u00c0-\uffff]$/i; } })();
  window.musterFold = function (s) {
    s = String(s == null ? '' : s);
    if (s.normalize) s = s.normalize('NFKD');
    return s.replace(RANK_MARK, '').replace(/\uFEFF/g, '').toLowerCase().replace(/\u03c2/g, '\u03c3')
      .replace(/^[\s\u0085]+|[\s\u0085]+$/g, '');
  };
  window.musterRank = function (label, query) {
    var q = Array.from(window.musterFold(query));
    if (!q.length) return 0;
    var l = Array.from(window.musterFold(label));
    function at(i) {
      if (i + q.length > l.length) return false;
      for (var j = 0; j < q.length; j++) if (l[i + j] !== q[j]) return false;
      return true;
    }
    if (at(0)) return 0;
    var found = false;
    for (var i = 1; i + q.length <= l.length; i++) {
      if (at(i)) {
        found = true;
        if (!RANK_WORD.test(l[i - 1])) return 1;
      }
    }
    if (found) return 2;
    var k = 0;
    for (var m = 0; m < l.length; m++) if (k < q.length && l[m] === q[k]) k++;
    return k === q.length ? 3 : -1;
  };
`
