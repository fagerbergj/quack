import { useState, useEffect } from 'react'

export default function Sidebar({ open, onClose }: { open: boolean; onClose: () => void }) {
  return (
    <div className={`fixed inset-y-0 left-0 z-30 w-64 flex-shrink-0 bg-gray-800 text-gray-100 transition-transform duration-200 md:relative md:translate-x-0 ${open ? 'translate-x-0' : '-translate-x-full md:translate-x-0'}`}>
      <div className="p-4 border-b border-gray-700 flex items-center justify-between">
        <h1 className="text-lg font-semibold">Agent Researcher</h1>
        <button onClick={onClose} className="md:hidden text-gray-400 hover:text-gray-200">
          ✕
        </button>
      </div>
      <nav className="p-2 space-y-1">
        <a href="/chat" className="flex items-center gap-3 px-3 py-2 rounded-md bg-gray-700 text-white">
          <span>💬</span> Chat
        </a>
      </nav>
    </div>
  )
}
