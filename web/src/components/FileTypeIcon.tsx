// FileTypeIcon — detects a file's type from its extension and renders a
// distinct colored icon for it (image, document, spreadsheet, code, archive,
// audio, video, pdf, etc). Falls back to a generic file icon for unknown
// extensions. Folders use a dedicated folder icon.

export type FileCategory =
  | 'folder'
  | 'image'
  | 'video'
  | 'audio'
  | 'pdf'
  | 'document'
  | 'spreadsheet'
  | 'presentation'
  | 'archive'
  | 'code'
  | 'text'
  | 'font'
  | 'generic'

const EXTENSION_MAP: Record<string, FileCategory> = {
  // Images
  png: 'image', jpg: 'image', jpeg: 'image', gif: 'image', svg: 'image',
  webp: 'image', bmp: 'image', ico: 'image', tiff: 'image', tif: 'image',
  heic: 'image', avif: 'image',
  // Video
  mp4: 'video', mov: 'video', avi: 'video', mkv: 'video', webm: 'video',
  flv: 'video', wmv: 'video', m4v: 'video',
  // Audio
  mp3: 'audio', wav: 'audio', flac: 'audio', aac: 'audio', ogg: 'audio',
  m4a: 'audio', wma: 'audio',
  // PDF
  pdf: 'pdf',
  // Documents
  doc: 'document', docx: 'document', odt: 'document', rtf: 'document',
  // Spreadsheets
  xls: 'spreadsheet', xlsx: 'spreadsheet', csv: 'spreadsheet', ods: 'spreadsheet', tsv: 'spreadsheet',
  // Presentations
  ppt: 'presentation', pptx: 'presentation', odp: 'presentation', key: 'presentation',
  // Archives
  zip: 'archive', tar: 'archive', gz: 'archive', tgz: 'archive', rar: 'archive',
  '7z': 'archive', bz2: 'archive', xz: 'archive',
  // Code
  js: 'code', jsx: 'code', ts: 'code', tsx: 'code', py: 'code', go: 'code',
  java: 'code', c: 'code', cpp: 'code', h: 'code', cs: 'code', rb: 'code',
  php: 'code', rs: 'code', swift: 'code', kt: 'code', sh: 'code', sql: 'code',
  yaml: 'code', yml: 'code', json: 'code', xml: 'code', html: 'code', css: 'code',
  // Text
  txt: 'text', md: 'text', log: 'text',
  // Fonts
  ttf: 'font', otf: 'font', woff: 'font', woff2: 'font',
}

const CATEGORY_COLOR: Record<FileCategory, string> = {
  folder: 'text-yellow-500',
  image: 'text-pink-500',
  video: 'text-purple-500',
  audio: 'text-fuchsia-500',
  pdf: 'text-red-500',
  document: 'text-blue-500',
  spreadsheet: 'text-green-600',
  presentation: 'text-orange-500',
  archive: 'text-amber-600',
  code: 'text-cyan-600',
  text: 'text-gray-400',
  font: 'text-indigo-400',
  generic: 'text-gray-400',
}

export function getFileCategory(name: string, isFolder?: boolean): FileCategory {
  if (isFolder) return 'folder'
  const dot = name.lastIndexOf('.')
  if (dot === -1 || dot === name.length - 1) return 'generic'
  const ext = name.slice(dot + 1).toLowerCase()
  return EXTENSION_MAP[ext] || 'generic'
}

function categoryColor(category: FileCategory): string {
  return CATEGORY_COLOR[category]
}

interface Props {
  name: string
  isFolder?: boolean
  className?: string
}

export default function FileTypeIcon({ name, isFolder, className = 'w-4 h-4' }: Props) {
  const category = getFileCategory(name, isFolder)
  const color = categoryColor(category)

  if (category === 'folder') {
    return (
      <svg className={`${className} ${color}`} fill="currentColor" viewBox="0 0 20 20">
        <path d="M2 6a2 2 0 012-2h5l2 2h5a2 2 0 012 2v6a2 2 0 01-2 2H4a2 2 0 01-2-2V6z" />
      </svg>
    )
  }

  // Shared document-shaped outline used as the base for most file categories,
  // with a category-specific glyph/badge layered on top.
  const base = (children: React.ReactNode) => (
    <svg className={`${className} ${color}`} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.5}>
      <path strokeLinecap="round" strokeLinejoin="round" d="M14.25 3.104v3.521a1.125 1.125 0 001.125 1.125h3.521M14.25 3H5.625C5.004 3 4.5 3.504 4.5 4.125v15.75c0 .621.504 1.125 1.125 1.125h12.75c.621 0 1.125-.504 1.125-1.125V8.25L14.25 3z" />
      {children}
    </svg>
  )

  switch (category) {
    case 'image':
      return base(<path strokeLinecap="round" strokeLinejoin="round" d="M8.25 16.5l2.25-2.25a1 1 0 011.4 0l2.6 2.6M9 12a1 1 0 100-2 1 1 0 000 2z" />)
    case 'video':
      return base(<path strokeLinecap="round" strokeLinejoin="round" d="M9.75 11.25l4 2.25-4 2.25v-4.5z" />)
    case 'audio':
      return base(<path strokeLinecap="round" strokeLinejoin="round" d="M9 16.5a1.5 1.5 0 11-3 0 1.5 1.5 0 013 0zm0 0V11l5-1v5.5m0 0a1.5 1.5 0 11-3 0 1.5 1.5 0 013 0z" />)
    case 'pdf':
      return base(<text x="7.2" y="17.5" fontSize="6.2" fontWeight="700" stroke="none" fill="currentColor">PDF</text>)
    case 'document':
      return base(<path strokeLinecap="round" strokeLinejoin="round" d="M8.25 12.75h7.5M8.25 15.75h7.5M8.25 9.75h3.5" />)
    case 'spreadsheet':
      return base(<path strokeLinecap="round" strokeLinejoin="round" d="M8.25 10.5h7.5v7.5h-7.5v-7.5zm0 2.5h7.5m-7.5 2.5h7.5m-5-5v7.5m2.5-7.5v7.5" />)
    case 'presentation':
      return base(<path strokeLinecap="round" strokeLinejoin="round" d="M8.25 11.25h7.5v4.5h-7.5v-4.5zM12 15.75v2.25" />)
    case 'archive':
      return base(<path strokeLinecap="round" strokeLinejoin="round" d="M10.5 9.75v.01M10.5 11.25v.01M10.5 12.75v.01M10.5 14.25v.01M10.5 15.75a1 1 0 102 0v-1.5h-2v1.5z" />)
    case 'code':
      return base(<path strokeLinecap="round" strokeLinejoin="round" d="M10 11l-2 2.25L10 15.5m4-4.5l2 2.25-2 2.25" />)
    case 'font':
      return base(<text x="7.6" y="17.5" fontSize="7" fontWeight="700" stroke="none" fill="currentColor">A</text>)
    case 'text':
      return base(<path strokeLinecap="round" strokeLinejoin="round" d="M8.25 12.75h7.5M8.25 15.75h5" />)
    default:
      return base(null)
  }
}
