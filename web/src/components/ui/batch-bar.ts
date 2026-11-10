/**
 * 选中后出现的批量操作条（bg-ink text-page）。浅色下是深底，深色下是浅底，
 * 所以条上的按钮颜色要跟着主题反过来，不能写死浅色字。
 * 深色主题的 --bad 是浅粉，在浅底上对比不够，所以深色下用浅色主题的那个红。
 */
export const batchBarClass = "flex flex-wrap items-center gap-1.5 rounded-[10px] bg-ink py-2 pr-2.5 pl-4 text-[13px] text-page";
export const batchBarDangerClass =
  "inline-flex items-center gap-1.5 rounded-[6px] bg-page/10 px-2.5 py-1 text-[#F1B4A8] hover:bg-page/15 disabled:opacity-50 dark:bg-[#B5483B]/10 dark:text-[#B5483B] dark:hover:bg-[#B5483B]/15";
