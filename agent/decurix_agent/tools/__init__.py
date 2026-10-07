"""
Generic document tools for the desktop's engines.

office: read any document (PDF, scan, Word, workbook, email), create and
update workbooks, combine files into a PDF, draft an email. outbox: hands a
draft to the control plane's Outbox for a person to send. Nothing here knows
about a particular workflow or system - that is for the engine, or a skill a
person taught, to work out.
"""