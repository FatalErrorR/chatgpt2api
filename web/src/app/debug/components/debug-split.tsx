/** 调试面板：左边请求，右边原始返回。返回用等宽字体。 */
export function DebugSplit({
  request,
  response,
  responseMeta,
}: {
  request: React.ReactNode;
  response: React.ReactNode;
  responseMeta?: React.ReactNode;
}) {
  return (
    <div className="grid items-start gap-3.5 lg:grid-cols-2">
      <section className="rounded-[10px] border border-line bg-surface">
        <div className="px-[18px] pt-3.5 pb-2">
          <h2 className="text-[17px] font-medium text-ink">请求</h2>
        </div>
        <div className="grid gap-3 px-[18px] pb-4">{request}</div>
      </section>
      <section className="rounded-[10px] border border-line bg-surface">
        <div className="flex items-center justify-between gap-3 px-[18px] pt-3.5 pb-2">
          <h2 className="text-[17px] font-medium text-ink">返回</h2>
          {responseMeta}
        </div>
        <div className="px-[18px] pb-4">{response}</div>
      </section>
    </div>
  );
}

export function DebugResponse({ children }: { children: React.ReactNode }) {
  return (
    <pre className="max-h-[70vh] min-h-[220px] overflow-auto rounded-[7px] border border-line bg-sunken p-3 font-mono text-xs leading-7 whitespace-pre-wrap text-ink-2">
      {children}
    </pre>
  );
}
