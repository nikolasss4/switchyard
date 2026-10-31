# Semantic cache: hit rate vs wrong-hit rate

`pairs.jsonl` holds 80 prompt pairs written by hand for this repo: 40 where a
cached answer to the first prompt would also answer the second (paraphrases,
case, punctuation), and 40 that look alike but need a different answer (swapped
entity, direction, number, year, polarity, scope, language).

`semeval` embeds every prompt, scores each pair by cosine similarity, and for
each threshold reports how many paraphrases would hit the cache and how many
different questions would wrongly get the cached answer.

```bash
# the mock's hashed bag-of-words embedding (a baseline, not a model)
go run ./cmd/semeval -hash
# a real model behind a local OpenAI-compatible endpoint
uv run --no-project --with sentence-transformers bench/semantic/embed_server.py sentence-transformers/all-MiniLM-L6-v2 8099 &
go run ./cmd/semeval -base-url http://127.0.0.1:8099/v1 -model all-MiniLM-L6-v2
```

Results are in `bench/results/semantic-*.md`. With 40 pairs per class, each
pair moves a rate by 2.5 points, so read the numbers as coarse.
