embedder: all-MiniLM-L6-v2, 40 paraphrase pairs, 40 different-question pairs

| threshold | hit rate (paraphrases) | wrong-hit rate (different questions) |
|---:|---:|---:|
| 0.70 | 98% (39/40) | 82% (33/40) |
| 0.75 | 98% (39/40) | 72% (29/40) |
| 0.80 | 92% (37/40) | 65% (26/40) |
| 0.85 | 92% (37/40) | 48% (19/40) |
| 0.88 | 85% (34/40) | 35% (14/40) |
| 0.90 | 72% (29/40) | 25% (10/40) |
| 0.92 | 68% (27/40) | 20% (8/40) |
| 0.94 | 55% (22/40) | 12% (5/40) |
| 0.95 | 40% (16/40) | 12% (5/40) |
| 0.96 | 30% (12/40) | 10% (4/40) |
| 0.97 | 22% (9/40) | 5% (2/40) |
| 0.98 | 12% (5/40) | 5% (2/40) |
| 0.99 | 8% (3/40) | 2% (1/40) |

most similar different-question pairs:
  0.996  direction    "What is the exchange rate from USD to EUR?" vs "What is the exchange rate from EUR to USD?"
  0.986  direction    "Convert 5 km to miles." vs "Convert 5 miles to km."
  0.968  direction    "Sort these numbers ascending: 5, 2, 9" vs "Sort these numbers descending: 5, 2, 9"
  0.966  attribute swap "How long should I boil an egg for a soft yolk?" vs "How long should I boil an egg for a hard yolk?"
  0.951  polarity     "Is coffee bad for your heart?" vs "Is coffee good for your heart?"
  0.934  attribute swap "Recommend a good beginner book on statistics." vs "Recommend a good advanced book on statistics."
  0.934  attribute swap "What is the GDP of Germany?" vs "What is the GDP per capita of Germany?"
  0.926  ordinal      "Who was the first person to walk on the moon?" vs "Who was the second person to walk on the moon?"
