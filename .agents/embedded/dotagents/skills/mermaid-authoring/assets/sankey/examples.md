# Sankey: realistic examples

## SaaS signup-to-paid funnel

What this shows: a conversion funnel expressed as flow volume dropping off at each stage — a more informative alternative to a bar-chart funnel because it shows where the drop-off goes (implicitly, to nothing).

```mermaid
sankey

Visitors,Signed up,4200
Signed up,Activated,2100
Activated,Trial started,1400
Trial started,Paid,380
```

## Cloud budget allocation

What this shows: budget flowing from a total pool down through departments into specific line items.

```mermaid
sankey

Total Budget,Engineering,1200000
Total Budget,Sales,650000
Total Budget,Marketing,400000
Engineering,Compute,700000
Engineering,Headcount,500000
Sales,Commissions,450000
Sales,Tools,200000
Marketing,Ads,300000
Marketing,Events,100000
```

## Support ticket routing between teams

What this shows: how volume moves between intake and resolution teams, with a comma-containing node name requiring quoting.

```mermaid
sankey

Intake,"Tier 1, General",800
Intake,Billing,150
"Tier 1, General",Resolved,600
"Tier 1, General",Escalated,200
Escalated,"Tier 2, Engineering",150
Escalated,Billing,50
Billing,Resolved,180
```

## Energy flow with custom node colors and no value labels

What this shows: `nodeColors` + `showValues: false` for a cleaner presentational chart where the flow widths, not the numbers, carry the message.

```mermaid
---
config:
  sankey:
    showValues: false
    nodeColors:
      Solar: "#f2b134"
      Wind: "#3ea6ff"
      Grid: "#7a7a7a"
      Losses: "#c0392b"
---
sankey

Solar,Grid,120
Wind,Grid,95
Grid,Homes,150
Grid,Industry,45
Grid,Losses,20
```
