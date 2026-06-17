import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import { ChatStoreProvider } from './state/ChatStoreProvider'
import './index.css'

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <ChatStoreProvider>
      <App />
    </ChatStoreProvider>
  </React.StrictMode>,
)
