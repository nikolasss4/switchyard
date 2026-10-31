embedder: bge-base-en-v1.5, 40 paraphrase pairs, 40 different-question pairs

| threshold | hit rate (paraphrases) | wrong-hit rate (different questions) |
|---:|---:|---:|
| 0.70 | 100% (40/40) | 95% (38/40) |
| 0.75 | 100% (40/40) | 95% (38/40) |
| 0.80 | 100% (40/40) | 88% (35/40) |
| 0.85 | 95% (38/40) | 62% (25/40) |
| 0.88 | 88% (35/40) | 52% (21/40) |
| 0.90 | 80% (32/40) | 32% (13/40) |
| 0.92 | 68% (27/40) | 25% (10/40) |
| 0.94 | 55% (22/40) | 12% (5/40) |
| 0.95 | 45% (18/40) | 10% (4/40) |
| 0.96 | 32% (13/40) | 8% (3/40) |
| 0.97 | 20% (8/40) | 5% (2/40) |
| 0.98 | 12% (5/40) | 5% (2/40) |
| 0.99 | 8% (3/40) | 2% (1/40) |

most similar different-question pairs:
  0.995  direction    "What is the exchange rate from USD to EUR?" vs "What is the exchange rate from EUR to USD?"
  0.985  direction    "Convert 5 km to miles." vs "Convert 5 miles to km."
  0.965  direction    "Sort these numbers ascending: 5, 2, 9" vs "Sort these numbers descending: 5, 2, 9"
  0.951  polarity     "Is coffee bad for your heart?" vs "Is coffee good for your heart?"
  0.941  attribute swap "How long should I boil an egg for a soft yolk?" vs "How long should I boil an egg for a hard yolk?"
  0.940  language     "How do I reverse a list in Python?" vs "How do I reverse a list in JavaScript?"
  0.936  entity swap  "How do I undo the last git commit?" vs "How do I undo the last git push?"
  0.926  attribute swap "What is the GDP of Germany?" vs "What is the GDP per capita of Germany?"
