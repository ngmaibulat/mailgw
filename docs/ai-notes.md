I suspect that with AI - we should use more straitforward code - which would be easier to analyze. And not abstractions over abstractions over abstractions

---

That's a really sharp observation, and it aligns with what a lot of developers are finding in practice.

**Why simpler code works better with AI:**

- AI reads code **linearly** — deep abstraction chains mean it has to chase through many files/layers to understand what's actually happening
- More files read = **more tokens consumed** per turn
- Abstractions often encode **implicit intent** that isn't obvious from the code itself — AI misses that context and makes wrong assumptions
- Simpler code = **smaller diffs** = less chance of Claude breaking something adjacent

**The irony:**
A lot of "best practices" (DRY, design patterns, heavy abstraction) were invented to help _humans_ manage complexity across time. AI doesn't have the same cognitive limits — it can handle repetition fine, but struggles with indirection.

**What tends to work well with AI:**

- Flat, explicit functions over deeply inherited class hierarchies
- Inline logic over clever metaprogramming
- Obvious naming over terse abbreviations
- Co-located code (things that change together, live together)

**What tends to work poorly:**

- Heavy decorator/mixin patterns
- Dynamic dispatch where behavior isn't obvious from reading
- Monkeypatching, heavy reflection
- "Magic" frameworks where a lot happens implicitly

There's a growing school of thought that **AI-assisted development is actually pushing back toward simpler, more explicit code** — which is arguably good for human readers too. Abstractions were always a tradeoff, and AI is shifting where that tradeoff lands.

What stack are you working with? Some patterns are more problematic than others depending on the language/framework.
