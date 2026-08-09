# Territory Builder

`postcodes.geojson` is a small **synthetic** dataset: seven schematic square
"postcode" polygons, loosely arranged over Bavaria for a plausible-looking
demo — not real PLZ boundaries. Swap it for a real postcode/administrative
boundary export (e.g. from OpenStreetMap `boundary=postal_code` relations,
or any national statistics office) to use these commands for real.

`territories.csv` maps each postcode to three independent, unrelated
business hierarchies at once, to show that grouping works on *any* mapping
column, not just one designated "territory" field.

## Sales: postcode → salesperson → sales territory

```bash
tinytiles territory \
  --input postcodes.geojson \
  --mapping territories.csv \
  --group-by sales_territory \
  --agg salesperson:unique \
  --output sales-territories.geojson
```

Produces one dissolved polygon per sales territory ("North", "South"), each
carrying `salesperson` (a list if a territory has more than one rep),
`area_km2`, and `source_feature_count`.

## Field service: postcode → technician/team → service region

```bash
tinytiles territory \
  --input postcodes.geojson \
  --mapping territories.csv \
  --group-by technician \
  --output service-regions.geojson
```

Grouping by `technician` instead of `service_region` gives each technician's
own coverage area directly — here Bob's area (84137 + 84140) is genuinely
disconnected, which the output correctly represents as a MultiPolygon with
two parts rather than merging or dropping one.

## Delivery logistics: postcode → depot → vehicle → delivery zone

```bash
tinytiles territory \
  --input postcodes.geojson \
  --mapping territories.csv \
  --group-by delivery_zone \
  --simplify 50m \
  --preset powerbi \
  --output delivery-zones.json
```

The `powerbi` preset simplifies geometry and drops the raw `source_values`
postcode list, producing a smaller file tuned for a Power BI shape-map
visual while keeping stable `territory_id`s and human-readable names.

## Convenience: prefix grouping without a mapping file

```bash
tinytiles territory --input postcodes.geojson --field postcode --group prefix:2 --output plz2.geojson
```

Groups postcodes purely by their first two digits (`84`, `94`) — no CSV
required.

## Validating and inspecting

```bash
tinytiles territory validate territories.csv
tinytiles territory validate --input postcodes.geojson --mapping territories.csv --group-by delivery_zone
tinytiles territory inspect sales-territories.geojson
```

`validate` reports unmatched geometries/mapping rows, duplicate mapping
keys, invalid geometries and possible overlaps as machine-readable JSON, and
exits non-zero if anything looks wrong. `inspect` summarizes any
Polygon/MultiPolygon GeoJSON file — its own output included.
