embedder: hashed bag-of-words (mock), 40 paraphrase pairs, 40 different-question pairs

| threshold | hit rate (paraphrases) | wrong-hit rate (different questions) |
|---:|---:|---:|
| 0.70 | 12% (5/40) | 92% (37/40) |
| 0.75 | 12% (5/40) | 88% (35/40) |
| 0.80 | 10% (4/40) | 60% (24/40) |
| 0.85 | 10% (4/40) | 38% (15/40) |
| 0.88 | 10% (4/40) | 15% (6/40) |
| 0.90 | 10% (4/40) | 8% (3/40) |
| 0.92 | 8% (3/40) | 5% (2/40) |
| 0.94 | 8% (3/40) | 0% (0/40) |
| 0.95 | 8% (3/40) | 0% (0/40) |
| 0.96 | 8% (3/40) | 0% (0/40) |
| 0.97 | 8% (3/40) | 0% (0/40) |
| 0.98 | 8% (3/40) | 0% (0/40) |
| 0.99 | 8% (3/40) | 0% (0/40) |

most similar different-question pairs:
  0.937  direction    "What is the exchange rate from USD to EUR?" vs "What is the exchange rate from EUR to USD?"
  0.925  scope        "How do I center a div with CSS?" vs "How do I center a div vertically with CSS?"
  0.912  scope        "How many days are in a leap year?" vs "How many days are in February in a leap year?"
  0.892  constraint   "Give me a recipe for pancakes." vs "Give me a recipe for gluten-free pancakes."
  0.889  attribute swap "How long should I boil an egg for a soft yolk?" vs "How long should I boil an egg for a hard yolk?"
  0.886  entity swap  "What should I do if my flight is delayed?" vs "What should I do if my flight is cancelled?"
  0.878  attribute swap "What is the boiling point of water at sea level?" vs "What is the freezing point of water at sea level?"
  0.875  direction    "Convert 5 km to miles." vs "Convert 5 miles to km."
