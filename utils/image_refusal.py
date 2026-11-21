"""判断上游的一段文字是不是「因内容政策拒绝这次生成」。

串流文字、任务列表、等图超时后的任务错误、会话文档里的助手文字都用这一套判断。
上游明确的拦截标记（SSE moderation blocked）由调用方先行采信；这里只处理只有文字的情况：
同一句里既要说违反了政策/准则/防护限制，又要有道歉或拒绝的语气，否定说法（没有违反）不算。
单独出现「内容政策」「无法生成」「policy」「blocked」这些词都不算拒绝。
"""
from __future__ import annotations

import re

# 「违反/不符合 …… 政策/准则/防护限制」：中间允许「了关于裸露、色情或情色内容的」这样的修饰
_ZH_VIOLATION_RE = re.compile(
    r"(违反|違反|违背|違背|触犯|觸犯|不符合|不符|有违|有違)[^。！？!?\n]{0,30}"
    r"(政策|准则|準則|规范|規範|规定|規定|防护限制|防護限制|使用条款|使用條款|安全限制|内容限制|內容限制)"
)
_EN_VIOLATION_RE = re.compile(
    r"\b(violat\w*|breach\w*|against|goes against|not (?:allowed|permitted) (?:by|under))\b[^.!?\n]{0,40}"
    r"\b(polic(?:y|ies)|guidelines?|terms of use|safety (?:rules|standards))\b"
    r"|\bblocked\b[^.!?\n]{0,20}\b(by|due to|under)\b[^.!?\n]{0,30}\b(polic(?:y|ies)|moderation|safety)\b",
    re.IGNORECASE,
)
# 紧挨在违规说法前面的否定：「没有违反」「不会违反」「并未违反」「does not violate」
_ZH_NEGATION_RE = re.compile(r"(没有|沒有|并没有|並沒有|并未|並未|不会|不會|不|未|无|無|避免|以免|确保不|確保不)\s*$")
_EN_NEGATION_RE = re.compile(r"\b(not|n't|never|no|avoid|without)\s*$", re.IGNORECASE)
# 道歉或拒绝的语气
_REFUSAL_RE = re.compile(
    r"抱歉|对不起|對不起|无法|無法|不能|不可以|拒绝|拒絕|未能|没能|沒能|不予"
    r"|\bsorry\b|\bapolog|\bcan(?:'|’)?t\b|\bcannot\b|\bunable\b|\bwon(?:'|’)?t\b|\bnot able\b|\bcouldn(?:'|’)?t\b"
    r"|\bcould not\b|\bwasn(?:'|’)?t able\b",
    re.IGNORECASE,
)
_SENTENCE_SPLIT_RE = re.compile(r"(?<=[。！？!?\n])|(?<=\.)\s+")


def _has_violation(sentence: str) -> bool:
    for pattern, negation in ((_ZH_VIOLATION_RE, _ZH_NEGATION_RE), (_EN_VIOLATION_RE, _EN_NEGATION_RE)):
        for match in pattern.finditer(sentence):
            if not negation.search(sentence[max(0, match.start() - 6):match.start()]):
                return True
    return False


def is_policy_refusal(text: str) -> bool:
    """整句是否表达「因政策拒绝这次生成」：违规说法 + 同一句或上一句里的道歉/拒绝语气。"""
    sentences = [part.strip() for part in _SENTENCE_SPLIT_RE.split(str(text or "")) if part and part.strip()]
    for index, sentence in enumerate(sentences):
        if not _has_violation(sentence):
            continue
        context = sentence if index == 0 else sentences[index - 1] + sentence
        if _REFUSAL_RE.search(context):
            return True
    return False
