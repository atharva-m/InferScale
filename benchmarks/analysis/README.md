# Benchmark analysis

Generate a comparison exclusively from measured result artifacts:

```bash
python benchmarks/analysis/render_report.py benchmarks/results/*.json --output benchmarks/results/report.md
```

The output intentionally contains no invented values. Plotting notebooks or scripts added later must preserve the result schema and provenance fields.
