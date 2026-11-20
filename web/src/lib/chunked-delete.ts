/** 每批删除的张数：一批一个请求，批与批之间更新进度。 */
export const DELETE_CHUNK_SIZE = 200;

export type DeleteProgress = { done: number; total: number; removed: number };

/**
 * 分批删除图片，每删完一批回调一次进度。某一批失败时停下，并把已经删掉的张数带在错误上。
 */
export async function deleteInChunks(
  paths: string[],
  remove: (chunk: string[]) => Promise<{ removed: number }>,
  onProgress: (progress: DeleteProgress) => void,
  chunkSize = DELETE_CHUNK_SIZE,
): Promise<number> {
  const total = paths.length;
  let removed = 0;
  onProgress({ done: 0, total, removed });
  for (let start = 0; start < total; start += chunkSize) {
    const chunk = paths.slice(start, start + chunkSize);
    try {
      removed += (await remove(chunk)).removed;
    } catch (error) {
      throw new ChunkedDeleteError(error instanceof Error ? error.message : "删除失败", removed);
    }
    onProgress({ done: Math.min(start + chunk.length, total), total, removed });
  }
  return removed;
}

export class ChunkedDeleteError extends Error {
  /** 失败前已经删掉的张数 */
  readonly removed: number;

  constructor(message: string, removed: number) {
    super(message);
    this.removed = removed;
  }
}
