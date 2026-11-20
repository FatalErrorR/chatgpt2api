/** 按自然日保留最近 days 天（含 today）：返回保留的最早日期。日期都是 YYYY-MM-DD，按 UTC 计算避免时区偏移。 */
export function retentionKeepFrom(today: string, days: number): string {
  return shiftDay(today, -(Math.max(1, Math.floor(days)) - 1));
}

/** 前一天，用来查「保留期之前」的图片（接口的 end_date 含当天）。 */
export function dayBefore(day: string): string {
  return shiftDay(day, -1);
}

function shiftDay(day: string, delta: number): string {
  const [year, month, date] = day.split("-").map(Number);
  return new Date(Date.UTC(year, month - 1, date + delta)).toISOString().slice(0, 10);
}

/** 输入框里的天数：1~3650 的整数，否则 null。 */
export function parseRetentionDays(value: string): number | null {
  const days = Number(value.trim());
  return Number.isInteger(days) && days >= 1 && days <= 3650 ? days : null;
}
