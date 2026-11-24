import '@testing-library/jest-dom/vitest'
import { cleanup, configure } from '@testing-library/react'
import { afterEach, vi } from 'vitest'

// The default 1s asyncUtilTimeout is too tight when vitest runs many jsdom
// environments in parallel and they compete for CPU; findBy*/waitFor then time
// out despite the component eventually rendering. 5s survives load.
configure({ asyncUtilTimeout: 5000 })

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})
