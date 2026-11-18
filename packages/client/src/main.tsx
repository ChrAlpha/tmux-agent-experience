import { render } from 'preact'
import './app.css'
import { start } from './store'
import { App } from './ui/App'

// The page fills the visual viewport, which a phone's keyboard shrinks while
// its layout viewport stays: the header and the composer stay on screen. A
// pinch zoom shrinks it too, and leaves the page as it was.
const fit = window.visualViewport
if (fit) {
  const follow = () => {
    if (Math.abs(fit.scale - 1) > 0.01) return
    document.documentElement.style.setProperty('--app-height', `${fit.height}px`)
    document.documentElement.style.setProperty('--app-top', `${fit.offsetTop}px`)
  }
  fit.addEventListener('resize', follow)
  fit.addEventListener('scroll', follow)
  follow()
}

render(<App />, document.body)
start()
