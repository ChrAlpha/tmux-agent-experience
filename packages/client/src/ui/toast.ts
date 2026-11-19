import { useEffect, useRef, useState } from 'preact/hooks'

export type Say = (text: string, error?: boolean) => void

// useToast holds a short message: a few seconds, longer for an error.
export function useToast(): [{ text: string; error: boolean }, Say] {
  const [toast, setToast] = useState({ text: '', error: false })
  const hide = useRef<ReturnType<typeof setTimeout>>(undefined)
  useEffect(() => () => clearTimeout(hide.current), [])
  const say: Say = (text, error = false) => {
    clearTimeout(hide.current)
    setToast({ text, error })
    hide.current = setTimeout(() => setToast({ text: '', error: false }), error ? 12_000 : 3_000)
  }
  return [toast, say]
}
