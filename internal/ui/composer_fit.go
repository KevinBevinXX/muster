package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE COMPOSERS GROW WITH WHAT IS TYPED — every message box in the app: the
// agent chat's #chat-input (and the agent panel, which renders the same pane)
// and the mirrored views' #mv-input (Rendered and Terminal). Both wear
// composerInputClass, which carries composerFitClass, so this is ONE
// implementation reached by both, not two copies.
//
// The rule: the box is as tall as its text, up to composerFitShare of the
// viewport's height, and scrolls inside itself past that. It shrinks back when
// its text is cleared (a send).
//
//   - CSS does the work where it can: `field-sizing: content` (web/css/input.css)
//     sizes a textarea to its content natively, and `max-height` caps it.
//   - The cap is `var(--composer-max-h, 40dvh)`. composerFitScript sets that
//     variable to 40% of the VISUAL viewport's height — on a phone, the part
//     above the on-screen keyboard. iOS Safari overlays the keyboard without
//     resizing the layout viewport, so 40dvh there would be 40% of a screen half
//     of which is keyboard; visualViewport is the only measure of what is
//     actually visible. Without script, 40dvh is the fallback.
//   - Where `field-sizing` is not supported (Safari before 26, Firefox), the
//     script sizes the box itself on every `input` event: height auto, then the
//     scroll height. max-height still caps it.
//
// 🔴 A VALUE SET BY SCRIPT FIRES NO `input` EVENT, so the fallback cannot see a
// send clear the box. The two senders (agentChatScript, mirrorComposerScript)
// call window.musterComposerFit(input) after emptying it; the agent panel's
// quick actions and dictation dispatch an `input` event instead, which the
// delegated listener here hears. With `field-sizing` none of that is needed —
// the box re-measures on any value change — which is why the e2e spec runs the
// growth and the shrink BOTH ways (native, and with field-sizing reported
// unsupported so this script's path is the one measured).
//
// ⚠ A PINCH IS NOT A RESIZE (composerPinScript's rule): a zoom shrinks
// visualViewport.height without the page being any shorter, so a reading at a
// scale other than 1 is ignored and the last good cap stays.
const (
	composerFitClass = "composer-fit"
	// composerFitShare is the cap, as a share of the visible viewport's height.
	// web/css/input.css spells the same 40 as its no-script fallback (40dvh);
	// TestComposerFitCapIsOneNumber holds the two together.
	composerFitShare = "0.4"
)

// composerFitScript is the composers' shared sizing: the cap variable and the
// fallback. Each composer ships it beside itself, because the documents they
// render into differ (the agent page, the agent panel's swapped body, the
// mirrored views). It binds once per document (a window flag), with
// document-level listeners, so a composer swapped in later is covered without
// binding again and every copy after the first is a no-op.
func composerFitScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.musterComposerFit) return;
  var SHARE = ` + composerFitShare + `;
  var CLS = '` + composerFitClass + `';
  var native = !!(window.CSS && CSS.supports && CSS.supports('field-sizing', 'content'));
  var root = document.documentElement;
  function cap() {
    var vv = window.visualViewport, h = window.innerHeight;
    if (vv) {
      if (Math.abs(vv.scale - 1) > 0.001) return;
      h = vv.height;
    }
    root.style.setProperty('--composer-max-h', Math.round(h * SHARE) + 'px');
  }
  // fit sizes one box. Natively there is nothing to do: field-sizing re-measures
  // on every value change. Otherwise: collapse, then grow to the content —
  // max-height (the cap) still applies, and past it the box scrolls.
  function fit(el) {
    if (!el || native || !el.classList || !el.classList.contains(CLS)) return;
    el.style.height = 'auto';
    el.style.height = el.scrollHeight + 'px';
  }
  window.musterComposerFit = fit;
  document.addEventListener('input', function (e) { fit(e.target); });
  window.addEventListener('resize', function () {
    cap();
    if (!native) [].forEach.call(document.querySelectorAll('textarea.' + CLS), fit);
  });
  if (window.visualViewport) window.visualViewport.addEventListener('resize', cap);
  cap();
})();
`))
}
