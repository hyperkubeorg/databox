// apps/pdf.js — the PDF previewer: the browser's own PDF renderer in an
// iframe over the inline file URL (application/pdf is inline-safe, see
// download.go). No pdf.js-the-library, no dependencies — every modern
// browser ships a viewer, and the iframe keeps it inside the app shell
// with the title/Download/Details chrome. Revision previews work for
// free: the host hands us a fileURL already pinned to the version.
export default function mount(root, ctx) {
  root.style.cssText += ';padding:0;overflow:hidden;display:flex;flex-direction:column';
  root.innerHTML = '';
  const frame = document.createElement('iframe');
  frame.src = ctx.fileURL;
  frame.title = ctx.name;
  frame.style.cssText = 'border:0;flex:1;width:100%;background:var(--surface-2);border-radius:14px';
  root.appendChild(frame);
}
