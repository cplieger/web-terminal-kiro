# Features

This page lists what the terminal supports on desktop and on touch, for readers checking whether their programs will work in it.

## A faithful terminal

The terminal comes from [web-terminal-engine](https://github.com/cplieger/web-terminal-engine). It shows kiro-cli's own screen as real browser text, so scrolling and text selection work the way they do on any web page.

- 16, 256 and 24-bit color, every text style including bold, italic, underline, reverse and strikethrough, box drawing, and wide CJK characters.
- Mouse support and clickable links that programs send over `OSC 8`. Bare addresses are turned into links too.
- Desktop notifications and progress indicators that programs send over `OSC 9` and `OSC 9;4`.
- Full-screen programs such as `vim`, `htop`, `less` and `man` run on their own screen, and your history comes back when they exit.
- Bracketed paste, a choice of cursor styles, the Kitty keyboard protocol, and clipboard writes from command-line programs over `OSC 52`.

## Made for touch

The page comes from [web-terminal-ui](https://github.com/cplieger/web-terminal-ui).

- Several tabs that you open, close and drag to reorder, plus a tab switcher you swipe on a phone.
- A two-pane split view. The split button in the tab row shows two sessions side by side with a divider you drag. Right-click or long-press a tab to snap it to either side, or drag it onto one half of the screen. The layout survives a reload and a switch to another device.
- An on-screen key bar for keys a phone keyboard lacks: Tab, Esc, the arrows, Enter, and a Ctrl key that stays pressed for the next letter.
- Native text selection, copy and paste, and a menu on long-press or right-click.
- Typing shows on screen before the server answers, so it feels instant over a slow link.
- Tap the terminal to focus it. A button returns to the bottom and follows new output again.
- A status dot on each tab shows whether its session is working, done or waiting for your answer. The waiting dot clears once the question is answered, denied or cancelled, including a question raised by a workflow step or a subagent.
- Input methods for composed characters, keyboard access, themes and reduced motion.

## Recovery after sleep and dropped connections

- The page reconnects on its own and replays the screen and history after a laptop sleeps, the network drops, or a proxy times out.
- Typing sent during an outage is delivered on reconnect, with nothing lost or doubled, and a restarted server is detected and the page resyncs.
- iOS often reclaims the memory of a tab in the background, which reloads the page when you come back. The sessions live on the server and each tab's recent lines are kept on the device, so the page asks only for what you missed. [Security](security.md#stored-scrollback) covers what that stores.

## kiro-cli features in the browser

The page drives kiro-cli's own terminal screen, so a feature kiro-cli shows on that screen works without any wiring in this app. That includes queue steering with `Ctrl+S`, goal runs with `/goal` and turn rewind with `/rewind`. On a phone, press Ctrl on the key bar, then the letter.
