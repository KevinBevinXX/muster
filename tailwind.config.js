/** @type {import('tailwindcss').Config} */
//
// muster Tailwind config.
//
// Dark mode is the DEFAULT: the app root (<html>) carries the `dark` class by
// default so the UI renders dark-first, with light as an opt-in toggle.
// `darkMode: 'class'` makes that switch class-driven rather than tied to the OS
// preference. Colour no longer needs the `dark:` variant at all: the palette is
// a set of custom properties that the class swaps (web/css/input.css).
//
// 🔴 TAILWIND DOES NOT ERROR ON A CONTENT PATH THAT MATCHES NOTHING. It emits
// no rules for it and exits 0, so a missing or mistyped entry produces a
// SMALLER, perfectly valid stylesheet and a page that renders unstyled — and it
// renders unstyled only where the built CSS is actually served, which is
// typically the container and not the developer's machine. There is no failure
// signal anywhere in the build. `make css-check` is that signal: it builds the
// stylesheet and then asserts that classes only reachable through each content
// entry are PRESENT in the output, so an entry that stopped matching fails
// loudly instead of shipping a thinner file.
// role(name) is the utility value for one palette token. See theme.extend.colors.
const role = (name) => `rgb(var(--mu-${name}) / <alpha-value>)`;

module.exports = {
  darkMode: 'class',
  content: [
    // The Go UI components. This is where essentially every class in the app
    // is written, so losing this entry is the catastrophic case: the build
    // still succeeds and the stylesheet still parses.
    './internal/ui/**/*.go',
    // 🔴 TEST FILES ARE EXCLUDED, AND THAT IS A DELETED PROBLEM RATHER THAN A
    // TIDY-UP. Without this line Tailwind scans _test.go files too, so any
    // class-shaped token in a fixture, an assertion or a doc comment is emitted
    // as a real rule and served to every device: a bare word like `shrink` or
    // `invert` in test prose, a Go slice expression like `x[start:end]` read as
    // an arbitrary-value class. The upstream project shipped 640 bytes of such
    // CSS and defended it with a hand-maintained ledger of accepted leaks, which
    // someone has to curate forever and which cannot distinguish an accident
    // from a decision.
    //
    // MEASURED on this tree, with the rest of the config unchanged:
    //     tests scanned    38,836 bytes   7 test-only classes
    //     tests excluded   38,274 bytes   0 test-only classes
    // and all five css-check control classes present in both, so the exclusion
    // removes exactly the leak and nothing else.
    //
    // ⚠ A NEGATED ENTRY IS LOAD-BEARING CONFIG, NOT A COMMENT. If a future
    // Tailwind drops support for `!` patterns it will not error — it will
    // silently go back to scanning test files, which is why
    // TestNoTestOnlyClassReachesTheStylesheet asserts on the OUTPUT rather than
    // on this line.
    '!./internal/ui/**/*_test.go',
    // Static HTML/JS for the PWA shell and any hand-written templates.
    './web/**/*.{html,js}',
    // 🔴 VENDORED THIRD-PARTY BUNDLES ARE EXCLUDED, AND THIS IS THE SAME
    // DELETED PROBLEM AS THE TEST-FILE LINE ABOVE, IN A NEW PLACE. The entry
    // above was written when web/ held only first-party files. The API carve
    // vendored five minified libraries into web/static/vendor/, and Tailwind
    // read every class-shaped token in them as a class to emit: htmx.min.js
    // contains its own `htmx-swapping` state class, and the Faro bundle
    // contains the word `isolate`. Both became real rules served to every
    // device, from source nobody in this repository wrote.
    //
    // MEASURED, with the rest of the config unchanged:
    //     vendor scanned    38,392 bytes   2 library-only classes
    //     vendor excluded   38,274 bytes   0 library-only classes
    // and all six css-check control classes present in both, so the exclusion
    // removes exactly the leak and nothing else.
    //
    // ⚠ THE FAILURE WAS FOUND BY css-check, NOT BY READING. The stylesheet was
    // still valid, still smaller than anything anyone would notice, and the
    // page still rendered. It surfaced only because that target compares the
    // build byte-for-byte against the committed copy.
    '!./web/static/vendor/**',
    // 🔴 THE ONE DOCUMENT-EMITTING FILE OUTSIDE internal/ui. The seam recorded
    // below is CLOSED by this line: internal/api/login.go renders the sign-in
    // page, which must display while the rest of the app is refusing and
    // therefore does not go through the component package.
    //
    // ⚠ THE FILE, NOT ITS PACKAGE, FOR THE REASON THE SEAM GAVE: an api package
    // is full of JSON, SQL and log strings, and a package-wide glob would also
    // scan _test.go files, where fixture data reads as arbitrary-value classes.
    // `text-balance` is in the css-check control set (Makefile) precisely so
    // this entry has a positive control rather than merely having been typed —
    // of every file this config scans, only login.go writes it.
    './internal/api/login.go',
    // ⚠ THE SECOND ONE (accounts PR 4): the gotrue-mode sign-in, admin claim
    // and account pages. It has NO control class of its own, and that is
    // stated rather than hidden: every class it writes is also written under
    // internal/ui or in login.go (measured: `make css` is byte-identical with
    // and without this line), so nothing it alone could prove is missing. The
    // first class it writes that no other entry does needs a css-check control.
    './internal/api/login_gotrue.go',
    // The THIRD and FOURTH (accounts PR 5): the invitee's page and the password
    // reset, and the admin pages (/admin/users, /admin/invites). Like the
    // second, NO control class of their own: measured, `make css` is
    // byte-identical with and without these two lines (a first draft's `py-5`
    // was the one class only admin.go wrote; it became the sign-in card's
    // `py-6`). The first class either writes that no other entry does needs a
    // css-check control.
    './internal/api/login_invite.go',
    './internal/api/admin.go',
    // Accounts PR 6: the /account/tokens page. Same status as the entry above
    // (measured by `make css` with and without it); a class only it writes
    // needs a css-check control.
    './internal/api/api_tokens.go',
    './internal/api/flows.go',
    //
    // ✅ SEAM CLOSED BY THE API CARVE — kept, rather than deleted, because it
    // states WHY the entry above is a file and not a glob. Re-read it before
    // widening that line.
    //
    // ⚠ ORIGINAL SEAM — A SECOND DOCUMENT SOURCE IS OWED BY THE API CARVE, AND
    // IT WILL FAIL SILENTLY IF NOBODY ADDS IT HERE.
    //
    //   WHAT: upstream emits one HTML document from OUTSIDE internal/ui — a
    //     hand-written login page that must render while the rest of the app is
    //     refusing, and therefore deliberately does not go through the component
    //     package. Its config names that ONE FILE explicitly.
    //   WHY NOT A PACKAGE-WIDE GLOB: upstream tried './internal/api/**/*.go'
    //     first. It scans _test.go files too, and Tailwind read fixture strings
    //     there as arbitrary-value classes — selectors built out of test data
    //     landed in the shipped stylesheet. An api package is full of JSON, SQL
    //     and log strings, so that recurs.
    //   CLOSING CONDITION: when the API carve lands a document-emitting file
    //     outside internal/ui, it adds that FILE (not its package) to this list
    //     and adds one of its classes to the css-check assertion set in the
    //     Makefile — so the entry is covered by a positive control rather than
    //     by having been typed.
    //   WHO CHECKS IT: the reviewer of the API-carve pull request. The
    //     mechanical check is `make css-check` failing before the entry is
    //     added and passing after.
  ],
  theme: {
    extend: {
      // 🔴 EVERY COLOUR IS A ROLE THAT READS A CUSTOM PROPERTY FROM
      // web/css/input.css. The palette (dark and light) is defined there ONCE;
      // this block only names the roles, so a theme change never touches the
      // views and the views never name a hue. `<alpha-value>` keeps the opacity
      // modifier working (`bg-bg/80`).
      //
      // ⚠ A ROLE ADDED HERE WITHOUT ITS VARIABLE IN input.css DOES NOT ERROR —
      // the browser resolves `rgb(var(--mu-missing))` to nothing and the element
      // simply loses its colour. TestEveryTailwindRoleHasATokenInBothThemes
      // (internal/ui/theme_test.go) reads this file and input.css and fails on
      // that disagreement.
      colors: {
        bg: role('bg'),
        s1: role('s1'),
        s2: role('s2'),
        s3: role('s3'),
        line: role('line'),
        edge: role('edge'),
        fg: role('text'),
        fg2: role('text2'),
        muted: role('muted'),
        accent: role('accent'),
        'on-accent': role('on-accent'),
        focus: role('focus'),
        danger: role('danger'),
        'on-danger': role('on-danger'),
        st: {
          open: { fg: role('st-open-fg'), bg: role('st-open-bg') },
          progress: { fg: role('st-progress-fg'), bg: role('st-progress-bg') },
          review: { fg: role('st-review-fg'), bg: role('st-review-bg') },
          complete: { fg: role('st-complete-fg'), bg: role('st-complete-bg') },
          error: { fg: role('st-error-fg'), bg: role('st-error-bg') },
          warning: { fg: role('st-warning-fg'), bg: role('st-warning-bg') },
          running: { fg: role('st-running-fg'), bg: role('st-running-bg') },
        },
      },
    },
  },
  plugins: [],
};
